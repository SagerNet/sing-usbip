//go:build linux || (darwin && cgo) || windows

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	usbip "github.com/sagernet/sing-usbip"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"

	"github.com/spf13/cobra"
)

const (
	listDialTimeout     = 10 * time.Second
	listExchangeTimeout = 30 * time.Second
)

var listCommand = &cobra.Command{
	Use:   "list",
	Short: "List local devices or devices exported by a server",
	Long: "List local devices or devices exported by a server.\n\n" +
		"By default, the local devices available for export are listed.\n" +
		"With --client, the devices exported by the server are listed.",
	RunE: runList,
	Args: cobra.NoArgs,
}

func init() {
	mainCommand.AddCommand(listCommand)
}

func runList(_ *cobra.Command, _ []string) error {
	if clientAddress == "" {
		return listLocalDevices()
	}
	remoteAddress, err := parseServerAddress(clientAddress, usbip.DefaultPort)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return listExportedDevices(ctx, remoteAddress)
}

func listLocalDevices() error {
	devices, err := usbip.ListLocalDevices()
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		fmt.Println("no local usb devices")
		return nil
	}
	entries := make([]usbip.DeviceEntry, len(devices))
	for i := range devices {
		entries[i] = devices[i].Entry
	}
	return printDeviceEntries(entries)
}

func listExportedDevices(ctx context.Context, remoteAddress M.Socksaddr) error {
	entries, err := fetchControlEntries(ctx, remoteAddress)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no exportable devices on " + remoteAddress.String())
		return nil
	}
	return printDeviceEntries(entries)
}

func dialListServer(ctx context.Context, remoteAddress M.Socksaddr) (net.Conn, error) {
	dialer := net.Dialer{Timeout: listDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", remoteAddress.String())
	if err != nil {
		return nil, E.Cause(err, "dial ", remoteAddress)
	}
	_ = conn.SetDeadline(time.Now().Add(listExchangeTimeout))
	return conn, nil
}

func fetchControlEntries(ctx context.Context, remoteAddress M.Socksaddr) ([]usbip.DeviceEntry, error) {
	conn, err := dialListServer(ctx, remoteAddress)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return usbip.FetchControlDeviceEntries(conn)
}

func printDeviceEntries(entries []usbip.DeviceEntry) error {
	slices.SortFunc(entries, func(a, b usbip.DeviceEntry) int {
		return strings.Compare(a.Info.BusIDString(), b.Info.BusIDString())
	})
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "BUSID\tVID:PID\tNAME\tSPEED\tSERIAL\tPATH")
	for i := range entries {
		info := &entries[i].Info
		fmt.Fprintln(writer, info.BusIDString()+"\t"+
			fmt.Sprintf("%04x:%04x", info.IDVendor, info.IDProduct)+"\t"+
			entries[i].Product+"\t"+
			speedName(info.Speed)+"\t"+
			entries[i].Serial+"\t"+
			info.PathString())
	}
	return writer.Flush()
}

func speedName(speed uint32) string {
	switch speed {
	case usbip.SpeedLow:
		return "low"
	case usbip.SpeedFull:
		return "full"
	case usbip.SpeedHigh:
		return "high"
	case usbip.SpeedWireless:
		return "wireless"
	case usbip.SpeedSuper:
		return "super"
	case usbip.SpeedSuperPlus:
		return "super+"
	default:
		return "unknown"
	}
}
