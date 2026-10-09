//go:build darwin && !ios && cgo

package usbip

import (
	"context"
	"errors"
	"time"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"

	"golang.org/x/sys/unix"
)

type darwinEndpoint struct {
	ctx    context.Context
	cancel context.CancelFunc
	logger logger.ContextLogger

	stateMachine *darwinUSBHostEndpointSM
	peer         *UsbIpPeer
	currentFrame func() uint64
	devID        uint32
	key          darwinEndpointKey

	commandCh  chan darwinCIMessage
	doorbellCh chan uint32
	workerDone chan struct{}

	setup    [8]byte
	setupSet bool
}

type pendingTransfer struct {
	transaction   *UrbTransaction
	transfer      darwinCITransfer
	direction     uint32
	requestLen    int
	bufferPtr     unsafe.Pointer
	noResponse    bool
	controlStatus bool
}

// darwinControlPauseGrace bounds how long an EndpointPause on a control
// endpoint waits for the in-flight request to finish before falling back to
// CMD_UNLINK. It stays well below the 2 s command timeout advertised in the
// controller capabilities.
const darwinControlPauseGrace = 500 * time.Millisecond

func newDarwinEndpoint(ctx context.Context, logger logger.ContextLogger, stateMachine *darwinUSBHostEndpointSM, peer *UsbIpPeer, currentFrame func() uint64, devID uint32, key darwinEndpointKey) *darwinEndpoint {
	ctx, cancel := context.WithCancel(ctx)
	e := &darwinEndpoint{
		ctx:          ctx,
		cancel:       cancel,
		logger:       logger,
		stateMachine: stateMachine,
		peer:         peer,
		currentFrame: currentFrame,
		devID:        devID,
		key:          key,
		commandCh:    make(chan darwinCIMessage, 4),
		doorbellCh:   make(chan uint32, 16),
		workerDone:   make(chan struct{}),
	}
	go e.worker()
	return e
}

func (e *darwinEndpoint) Command(message darwinCIMessage) {
	select {
	case e.commandCh <- message:
	case <-e.ctx.Done():
	}
}

func (e *darwinEndpoint) EnqueueDoorbell(doorbell uint32) {
	select {
	case e.doorbellCh <- doorbell:
	case <-e.ctx.Done():
	default:
		e.logger.Debug("endpoint doorbell queue overflow ", e.key.device, ".", e.key.endpoint)
	}
}

func (e *darwinEndpoint) Close() {
	e.cancel()
	<-e.workerDone
}

func (e *darwinEndpoint) Wait() {
	<-e.workerDone
}

func (e *darwinEndpoint) worker() {
	defer close(e.workerDone)
	defer e.stateMachine.Close()

	var pending *pendingTransfer
	var pendingDoorbell bool
	var previousNoResponse unsafe.Pointer

	for {
		if pending != nil {
			select {
			case <-e.ctx.Done():
				e.abortPending(pending, false)
				return
			case message := <-e.commandCh:
				done := e.handleCommand(message, pending)
				pending = nil
				if done {
					return
				}
				pendingDoorbell = false
				previousNoResponse = nil
			case <-pending.transaction.Done():
				e.finalizePending(pending)
				pending = nil
				pendingDoorbell = true
			}
			continue
		}

		if pendingDoorbell {
			transfer := e.stateMachine.currentTransfer()
			if transfer.ptr == nil || !transfer.message.valid() {
				pendingDoorbell = false
				continue
			}
			noResponse := transfer.message.control&(1<<14) != 0
			if noResponse {
				if transfer.ptr == previousNoResponse {
					pendingDoorbell = false
					continue
				}
				previousNoResponse = transfer.ptr
			} else {
				previousNoResponse = nil
			}
			pending = e.startTransfer(transfer, noResponse)
			continue
		}

		select {
		case <-e.ctx.Done():
			return
		case message := <-e.commandCh:
			if e.handleCommand(message, nil) {
				return
			}
		case doorbell := <-e.doorbellCh:
			err := e.stateMachine.processDoorbell(doorbell)
			if err != nil {
				e.logger.Debug("process doorbell: ", err)
				continue
			}
			pendingDoorbell = true
		}
	}
}

// handleCommand inspects the command before retiring the in-flight transfer,
// so the state machine knows the endpoint is being paused or destroyed when
// the transfer's completion is enqueued, then responds.
func (e *darwinEndpoint) handleCommand(message darwinCIMessage, pending *pendingTransfer) bool {
	inspectErr := e.stateMachine.inspect(message)
	if pending != nil {
		e.retirePending(pending, message)
	}
	if inspectErr != nil {
		e.logger.Debug("inspect endpoint command: ", inspectErr)
	} else {
		status := ciStatusSuccess
		if message.messageType() == ciMsgEndpointPause && e.stateMachine.state() == ciEndpointStateHalted {
			// The request in flight failed (for example a STALLed
			// GET_DESCRIPTOR) while the pause was pending, so its completion
			// halted the endpoint and it can no longer become Paused. The
			// state machine refuses a Success response here and an
			// unanswered command is fatal.
			status = ciStatusNotPermitted
		}
		err := e.stateMachine.respondInspected(message, status)
		if err != nil {
			e.logger.Debug("respond endpoint command: ", err)
		}
	}
	return message.messageType() == ciMsgEndpointDestroy
}

// retirePending quiesces the transfer in flight when a command arrives. A
// control request that is already on its way to the device (for example the
// SET_CONFIGURATION the kernel pauses endpoint 0 around) is allowed to finish
// and reports its real status: canceling it leaves the device state unknown,
// and the kernel later resumes the stopped status stage, which then cannot
// be replayed faithfully.
func (e *darwinEndpoint) retirePending(pending *pendingTransfer, message darwinCIMessage) {
	if e.key.endpoint == 0 && message.messageType() == ciMsgEndpointPause {
		timer := time.NewTimer(darwinControlPauseGrace)
		defer timer.Stop()
		select {
		case <-pending.transaction.Done():
			e.finalizePending(pending)
			return
		case <-e.peer.Done():
			e.finalizePending(pending)
			return
		case <-timer.C:
		}
	}
	e.abortPending(pending, true)
}

// A transfer retired by a driver command (EndpointPause and its follow-ups)
// must complete with EndpointStopped: any other non-success status transitions
// the endpoint to Halted.
func (e *darwinEndpoint) abortPending(pending *pendingTransfer, retiredByCommand bool) {
	_ = pending.transaction.Cancel()
	select {
	case <-pending.transaction.Done():
	case <-e.peer.Done():
	}
	if !retiredByCommand {
		e.finalizePending(pending)
		return
	}
	_, err := pending.transaction.Wait()
	if err == nil {
		e.finalizePending(pending)
		return
	}
	if pending.noResponse {
		return
	}
	completeErr := e.stateMachine.complete(pending.transfer, ciStatusEndpointStopped, 0)
	if completeErr != nil {
		e.logger.Debug("complete stopped transfer: ", completeErr)
		e.cancel()
	}
}

func (e *darwinEndpoint) finalizePending(pending *pendingTransfer) {
	response, err := pending.transaction.Wait()
	var status int32
	var length int
	switch {
	case errors.Is(err, ErrCanceled):
		status = -int32(unix.ECONNRESET)
		length = 0
	case errors.Is(err, ErrPeerClosed):
		status = -int32(unix.ENODEV)
		length = 0
	case err != nil:
		status = -int32(unix.EIO)
		length = 0
	default:
		var acceptErr error
		status, length, acceptErr = pending.accept(response)
		if acceptErr != nil {
			e.logger.Debug("RET_SUBMIT validation: ", acceptErr, " (request length ", pending.requestLen, ")")
			e.cancel()
		}
	}
	if pending.controlStatus {
		e.setupSet = false
	}
	if pending.noResponse {
		return
	}
	completeErr := e.stateMachine.complete(pending.transfer, darwinUSBIPStatusToCIStatus(status), length)
	if completeErr != nil {
		e.logger.Debug("complete transfer: ", completeErr)
		e.cancel()
	}
}

func (p *pendingTransfer) validateResponse(response SubmitResponse) (int32, error) {
	if response.ActualLength < 0 {
		return -int32(unix.EPROTO), E.New("RET_SUBMIT actual_length is negative: ", response.ActualLength)
	}
	if int(response.ActualLength) > p.requestLen {
		return -int32(unix.EOVERFLOW), E.New("RET_SUBMIT actual_length exceeds request length: actual_length ", response.ActualLength, ", request ", p.requestLen)
	}
	if p.direction != USBIPDirIn {
		return 0, nil
	}
	if len(response.Buffer) > p.requestLen {
		return -int32(unix.EOVERFLOW), E.New("RET_SUBMIT buffer exceeds request length: buffer ", len(response.Buffer), ", request ", p.requestLen)
	}
	if len(response.IsoPackets) > 0 {
		err := ValidateIsoResponse(p.requestLen, int(response.ActualLength), response.IsoPackets, len(response.Buffer))
		if err != nil {
			return -int32(unix.EPROTO), err
		}
	}
	return 0, nil
}

func (p *pendingTransfer) accept(response SubmitResponse) (int32, int, error) {
	errStatus, err := p.validateResponse(response)
	if err != nil {
		return errStatus, 0, err
	}
	actualLength := int(response.ActualLength)
	if p.direction != USBIPDirIn {
		return response.Status, actualLength, nil
	}
	copyLength := min(actualLength, len(response.Buffer))
	if copyLength > 0 && p.bufferPtr != nil {
		dst := unsafe.Slice((*byte)(p.bufferPtr), p.requestLen)
		if len(response.IsoPackets) > 0 {
			ScatterIsoResponse(dst, response.Buffer[:copyLength], response.IsoPackets)
		} else {
			copy(dst[:copyLength], response.Buffer[:copyLength])
		}
	}
	return response.Status, actualLength, nil
}

func (e *darwinEndpoint) startTransfer(transfer darwinCITransfer, noResponse bool) *pendingTransfer {
	message := transfer.message
	switch message.messageType() {
	case ciMsgSetupTransfer:
		e.setup = message.setup()
		e.setupSet = true
		if !noResponse {
			err := e.stateMachine.complete(transfer, ciStatusSuccess, 0)
			if err != nil {
				e.logger.Debug("complete setup transfer: ", err)
				e.cancel()
			}
		}
		return nil
	case ciMsgStatusTransfer:
		return e.startControlStatusTransfer(transfer, noResponse)
	case ciMsgNormalTransfer:
		if e.key.endpoint == 0 {
			return e.startControlDataTransfer(transfer, message, noResponse)
		}
		return e.startNormalTransfer(transfer, message, noResponse)
	case ciMsgIsochronousTransfer:
		return e.startIsoTransfer(transfer, message, noResponse)
	default:
		if !noResponse {
			_ = e.stateMachine.complete(transfer, darwinUSBIPStatusToCIStatus(-int32(unix.EIO)), 0)
		}
		return nil
	}
}

func (e *darwinEndpoint) startControlStatusTransfer(transfer darwinCITransfer, noResponse bool) *pendingTransfer {
	if !e.setupSet {
		if !noResponse {
			_ = e.stateMachine.complete(transfer, ciStatusSuccess, 0)
		}
		return nil
	}
	// The setup packet is kept until the status stage completes, so a status
	// stage stopped by EndpointPause and later resumed re-issues the request
	// instead of reporting success for a request that was canceled.
	setup := e.setup
	if setup[6] != 0 || setup[7] != 0 {
		e.setupSet = false
		if !noResponse {
			_ = e.stateMachine.complete(transfer, ciStatusSuccess, 0)
		}
		return nil
	}
	transaction, err := e.peer.Submit(SubmitCommand{
		Header: DataHeader{
			Command:   CmdSubmit,
			DevID:     e.devID,
			Direction: USBIPDirOut,
			Endpoint:  0,
		},
		NumberOfPackets: nonIsoPacketCount,
		Setup:           setup,
	})
	if err != nil {
		if !noResponse {
			_ = e.stateMachine.complete(transfer, darwinUSBIPStatusToCIStatus(-int32(unix.EIO)), 0)
		}
		return nil
	}
	return &pendingTransfer{
		transaction:   transaction,
		transfer:      transfer,
		direction:     USBIPDirOut,
		noResponse:    noResponse,
		controlStatus: true,
	}
}

func (e *darwinEndpoint) startControlDataTransfer(transfer darwinCITransfer, message darwinCIMessage, noResponse bool) *pendingTransfer {
	if !e.setupSet {
		if !noResponse {
			_ = e.stateMachine.complete(transfer, darwinUSBIPStatusToCIStatus(-int32(unix.EPROTO)), 0)
		}
		return nil
	}
	setup := e.setup
	length := int(message.normalLength())
	direction := USBIPDirOut
	var buffer []byte
	if setup[0]&0x80 != 0 {
		direction = USBIPDirIn
	} else {
		buffer = bytesFromUnsafe(message.bufferPointer(), length)
	}
	transaction, err := e.peer.Submit(SubmitCommand{
		Header: DataHeader{
			Command:   CmdSubmit,
			DevID:     e.devID,
			Direction: direction,
			Endpoint:  0,
		},
		TransferBufferLength: int32(length),
		NumberOfPackets:      nonIsoPacketCount,
		Setup:                setup,
		Buffer:               buffer,
	})
	if err != nil {
		if !noResponse {
			_ = e.stateMachine.complete(transfer, darwinUSBIPStatusToCIStatus(-int32(unix.EIO)), 0)
		}
		return nil
	}
	return &pendingTransfer{
		transaction: transaction,
		transfer:    transfer,
		direction:   direction,
		requestLen:  length,
		bufferPtr:   message.bufferPointer(),
		noResponse:  noResponse,
	}
}

func (e *darwinEndpoint) startNormalTransfer(transfer darwinCITransfer, message darwinCIMessage, noResponse bool) *pendingTransfer {
	length := int(message.normalLength())
	direction := USBIPDirOut
	var buffer []byte
	if e.key.endpoint&0x80 != 0 {
		direction = USBIPDirIn
	} else {
		buffer = bytesFromUnsafe(message.bufferPointer(), length)
	}
	transaction, err := e.peer.Submit(SubmitCommand{
		Header: DataHeader{
			Command:   CmdSubmit,
			DevID:     e.devID,
			Direction: direction,
			Endpoint:  uint32(e.key.endpoint & 0x0f),
		},
		TransferBufferLength: int32(length),
		NumberOfPackets:      nonIsoPacketCount,
		Buffer:               buffer,
	})
	if err != nil {
		if !noResponse {
			_ = e.stateMachine.complete(transfer, darwinUSBIPStatusToCIStatus(-int32(unix.EIO)), 0)
		}
		return nil
	}
	return &pendingTransfer{
		transaction: transaction,
		transfer:    transfer,
		direction:   direction,
		requestLen:  length,
		bufferPtr:   message.bufferPointer(),
		noResponse:  noResponse,
	}
}

func (e *darwinEndpoint) startIsoTransfer(transfer darwinCITransfer, message darwinCIMessage, noResponse bool) *pendingTransfer {
	length := int(message.normalLength())
	direction := USBIPDirOut
	var buffer []byte
	if e.key.endpoint&0x80 != 0 {
		direction = USBIPDirIn
	} else {
		buffer = bytesFromUnsafe(message.bufferPointer(), length)
	}
	ciFrame := uint8(message.control >> ciIsochronousTransferControlFramePhase)
	asap := message.control&ciIsochronousTransferControlASAP != 0
	command := EncodeIsoSubmit(e.currentFrame(), SubmitCommand{
		Header: DataHeader{
			Command:   CmdSubmit,
			DevID:     e.devID,
			Direction: direction,
			Endpoint:  uint32(e.key.endpoint & 0x0f),
		},
		TransferBufferLength: int32(length),
		NumberOfPackets:      1,
		Buffer:               buffer,
		IsoPackets: []IsoPacketDescriptor{{
			Offset: 0,
			Length: int32(length),
		}},
	}, ciFrame, asap)
	transaction, err := e.peer.Submit(command)
	if err != nil {
		if !noResponse {
			_ = e.stateMachine.complete(transfer, darwinUSBIPStatusToCIStatus(-int32(unix.EIO)), 0)
		}
		return nil
	}
	return &pendingTransfer{
		transaction: transaction,
		transfer:    transfer,
		direction:   direction,
		requestLen:  length,
		bufferPtr:   message.bufferPointer(),
		noResponse:  noResponse,
	}
}

func bytesFromUnsafe(ptr unsafe.Pointer, length int) []byte {
	if ptr == nil || length == 0 {
		return nil
	}
	out := make([]byte, length)
	copy(out, unsafe.Slice((*byte)(ptr), length))
	return out
}
