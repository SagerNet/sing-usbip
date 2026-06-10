//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// After CMD_UNLINK the Linux server replies only with RET_UNLINK (status
// ECONNRESET), never a parallel RET_SUBMIT.
func TestUrbTransactionCancelIdempotent(t *testing.T) {
	peer, server, _ := newPeerPair(t)

	var unlinks atomic.Int32
	var serverDone sync.WaitGroup
	serverDone.Add(1)
	go func() {
		defer serverDone.Done()
		submit := server.readSubmit(t)
		unlink := server.readUnlink(t)
		unlinks.Add(1)
		require.Equal(t, submit.Header.SeqNum, unlink.SeqNum)
		require.Equal(t, submit.Header.DevID, unlink.Header.DevID)
		server.writeUnlinkResponse(t, unlink.Header.SeqNum, usbipStatusECONNRESET)
	}()

	transaction, err := peer.Submit(SubmitCommand{
		Header: DataHeader{
			Command:   CmdSubmit,
			DevID:     0xCAFEF00D,
			Direction: USBIPDirIn,
			Endpoint:  1,
		},
		TransferBufferLength: 8,
	})
	require.NoError(t, err)

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			err := transaction.Cancel()
			require.NoError(t, err)
		}()
	}
	wg.Wait()

	_, err = transaction.Wait()
	require.ErrorIs(t, err, ErrCanceled)

	serverDone.Wait()
	require.Equal(t, int32(1), unlinks.Load(), "Cancel wrote CMD_UNLINK more than once")
}

// Linux's stub_recv_cmd_unlink race: the URB completes before CMD_UNLINK
// arrives; the server emits RET_SUBMIT then RET_UNLINK with status 0.
func TestUrbTransactionCancelAfterUrbCompleted(t *testing.T) {
	peer, server, _ := newPeerPair(t)

	var serverDone sync.WaitGroup
	serverDone.Add(1)
	go func() {
		defer serverDone.Done()
		submit := server.readSubmit(t)
		unlink := server.readUnlink(t)
		require.Equal(t, submit.Header.SeqNum, unlink.SeqNum)
		server.writeSubmitResponse(t, USBIPDirIn, submit.Header.SeqNum, 0, []byte{1, 2, 3, 4}, nil)
		server.writeUnlinkResponse(t, unlink.Header.SeqNum, 0)
	}()

	transaction, err := peer.Submit(SubmitCommand{
		Header: DataHeader{
			Command:   CmdSubmit,
			DevID:     1,
			Direction: USBIPDirIn,
			Endpoint:  1,
		},
		TransferBufferLength: 4,
	})
	require.NoError(t, err)

	require.NoError(t, transaction.Cancel())

	_, err = transaction.Wait()
	require.ErrorIs(t, err, ErrCanceled)

	serverDone.Wait()

	require.NoError(t, peer.Close())
}

// Linux stub_rx valid_request check: CMD_UNLINK must carry the original submit's DevID.
func TestUrbTransactionCancelWireCarriesDevID(t *testing.T) {
	peer, server, _ := newPeerPair(t)

	var serverDone sync.WaitGroup
	serverDone.Add(1)
	go func() {
		defer serverDone.Done()
		submit := server.readSubmit(t)
		unlink := server.readUnlink(t)
		require.Equal(t, uint32(0xDEADBEEF), unlink.Header.DevID)
		require.Equal(t, submit.Header.SeqNum, unlink.SeqNum)
		server.writeUnlinkResponse(t, unlink.Header.SeqNum, usbipStatusECONNRESET)
	}()

	transaction, err := peer.Submit(SubmitCommand{
		Header: DataHeader{
			Command:   CmdSubmit,
			DevID:     0xDEADBEEF,
			Direction: USBIPDirOut,
			Endpoint:  2,
		},
		TransferBufferLength: 4,
		Buffer:               []byte{5, 6, 7, 8},
	})
	require.NoError(t, err)

	require.NoError(t, transaction.Cancel())

	_, err = transaction.Wait()
	require.ErrorIs(t, err, ErrCanceled)

	serverDone.Wait()
}

func TestUrbTransactionCancelAfterTerminalNoWire(t *testing.T) {
	peer, server, _ := newPeerPair(t)

	var serverDone sync.WaitGroup
	serverDone.Add(1)
	go func() {
		defer serverDone.Done()
		submit := server.readSubmit(t)
		server.writeSubmitResponse(t, USBIPDirOut, submit.Header.SeqNum, 0, nil, nil)
		_, err := ReadDataHeader(server.conn)
		require.Error(t, err)
	}()

	transaction, err := peer.Submit(SubmitCommand{
		Header: DataHeader{
			Command:   CmdSubmit,
			DevID:     1,
			Direction: USBIPDirOut,
			Endpoint:  1,
		},
		TransferBufferLength: 4,
		Buffer:               []byte{1, 2, 3, 4},
	})
	require.NoError(t, err)

	_, err = transaction.Wait()
	require.NoError(t, err)

	require.NoError(t, transaction.Cancel())

	require.NoError(t, peer.Close())
	serverDone.Wait()
}
