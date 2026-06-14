//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func testControlDeviceEntry() DeviceEntry {
	var info DeviceInfoTruncated
	encodePathField(&info.Path, "/sys/bus/usb/devices/1-2")
	copy(info.BusID[:], "1-2")
	info.BusNum = 1
	info.DevNum = 2
	info.Speed = SpeedHigh
	info.IDVendor = 0x046d
	info.IDProduct = 0xc52b
	info.BCDDevice = 0x1201
	info.BConfigurationValue = 1
	info.BNumConfigurations = 1
	info.BNumInterfaces = 1
	return DeviceEntry{
		Info:    info,
		Serial:  "ABC123",
		Product: "Unifying Receiver",
		Interfaces: []DeviceInterface{{
			BInterfaceClass:    0x03,
			BInterfaceSubClass: 0x01,
			BInterfaceProtocol: 0x01,
		}},
	}
}

func TestControlDeviceInfoRoundTrip(t *testing.T) {
	t.Parallel()
	entry := testControlDeviceEntry()
	info := controlDeviceInfoFromEntry(entry, BackendIDLinuxSysfs, "stable-1", DeviceStateIdle, 0, "")
	require.Equal(t, "ABC123", info.Serial)
	require.Equal(t, "Unifying Receiver", info.Product)
	require.Equal(t, "/sys/bus/usb/devices/1-2", info.Path)

	entries := controlDeviceInfoToEntries([]ControlDeviceInfo{info}, true)
	require.Len(t, entries, 1)
	require.Equal(t, entry.Info, entries[0].Info)
	require.Equal(t, entry.Interfaces, entries[0].Interfaces)
	require.Equal(t, entry.Serial, entries[0].Serial)
	require.Equal(t, entry.Product, entries[0].Product)
}

func TestFetchControlDeviceEntries(t *testing.T) {
	t.Parallel()
	entry := testControlDeviceEntry()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	go func() {
		preface := make([]byte, len(controlPreface))
		_, err := io.ReadFull(serverConn, preface)
		if err != nil {
			return
		}
		var reader controlReader
		_, err = reader.read(serverConn)
		if err != nil {
			return
		}
		err = writeControlMessage(serverConn, controlFrame{
			Type:    controlFrameAck,
			Version: controlProtocolVersion,
		}, nil)
		if err != nil {
			return
		}
		_ = writeControlMessage(serverConn, controlFrame{
			Type:    controlFrameDeviceSnapshot,
			Version: controlProtocolVersion,
		}, controlDeviceSnapshot{
			Devices: []ControlDeviceInfo{
				controlDeviceInfoFromEntry(entry, BackendIDLinuxSysfs, "stable-1", DeviceStateIdle, 0, ""),
				controlDeviceInfoFromEntry(entry, BackendIDLinuxSysfs, "stable-2", DeviceStateAttached, 0, "used"),
			},
		})
	}()
	entries, err := FetchControlDeviceEntries(clientConn)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, entry.Info, entries[0].Info)
	require.Equal(t, entry.Serial, entries[0].Serial)
	require.Equal(t, entry.Product, entries[0].Product)
}
