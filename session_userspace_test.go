//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"context"
	"net"
	"testing"
	"time"

	E "github.com/sagernet/sing/common/exceptions"

	"github.com/stretchr/testify/require"
)

type deviceGoneEngine struct {
	submits chan struct{}
}

func (e *deviceGoneEngine) Submit(URBRequest) URBResponse {
	select {
	case e.submits <- struct{}{}:
	default:
	}
	return URBResponse{Error: E.New("device detached")}
}

func (e *deviceGoneEngine) AbortEndpoint(uint8) error { return nil }

func (e *deviceGoneEngine) Close() error { return nil }

func TestUserspaceSessionTearsDownOnDeviceGone(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := &deviceGoneEngine{submits: make(chan struct{}, 1)}
	session := newUserspaceURBSession(ctx, nopLogger(), serverConn, engine)
	require.NoError(t, session.Start())
	defer session.Close()

	responses := make(chan SubmitResponse, 1)
	go func() {
		err := WriteSubmitCommand(clientConn, SubmitCommand{
			Header: DataHeader{
				Command:   CmdSubmit,
				SeqNum:    555,
				DevID:     7,
				Direction: USBIPDirOut,
				Endpoint:  1,
			},
		})
		if err != nil {
			return
		}
		header, err := ReadDataHeader(clientConn)
		if err != nil {
			return
		}
		response, err := ReadSubmitResponseBody(clientConn, header, USBIPDirOut)
		if err != nil {
			return
		}
		responses <- response
	}()

	select {
	case response := <-responses:
		require.Equal(t, RetSubmit, response.Header.Command)
		require.Equal(t, uint32(555), response.Header.SeqNum)
		require.Equal(t, int32(usbipStatusEIO), response.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RET_SUBMIT")
	}

	select {
	case <-session.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session did not tear down after device-gone submit")
	}
	require.Error(t, session.Err())
}
