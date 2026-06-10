//go:build linux || (darwin && cgo) || windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/sagernet/sing-box/log"
	usbip "github.com/sagernet/sing-usbip"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/spf13/cobra"
)

var deviceMatches []string

var runCommand = &cobra.Command{
	Use:   "run",
	Short: "Run the USB/IP server or client until interrupted",
	Long: "Run the USB/IP server or client until interrupted.\n\n" +
		"With --server, the matched devices are exported; at least one --device is\n" +
		"required. With --client, the matched devices are imported; without --device,\n" +
		"every device exported by the server is imported.",
	RunE: runService,
	Args: cobra.NoArgs,
}

func init() {
	runCommand.Flags().StringArrayVarP(&deviceMatches, "device", "d", nil, "device to export or import (repeatable)")
	mainCommand.AddCommand(runCommand)
}

func runService(_ *cobra.Command, _ []string) error {
	devices, err := parseDeviceMatches(deviceMatches)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch {
	case serverAddress != "":
		return runServer(ctx, devices)
	case clientAddress != "":
		return runClient(ctx, devices)
	default:
		return E.New("one of --server or --client is required")
	}
}

func runServer(ctx context.Context, devices []usbip.DeviceMatch) error {
	if len(devices) == 0 {
		return E.New("at least one --device match is required")
	}
	listenAddress, err := parseListenAddress(serverAddress, usbip.DefaultPort)
	if err != nil {
		return err
	}
	service, err := usbip.NewServerService(ctx, usbip.ServerOptions{
		Logger:        log.StdLogger(),
		Devices:       devices,
		ListenAddress: listenAddress,
	})
	if err != nil {
		return err
	}
	err = service.Start()
	if err != nil {
		_ = service.Close()
		return err
	}
	for _, match := range devices {
		log.Info("exporting ", formatDeviceMatch(match))
	}
	<-ctx.Done()
	return service.Close()
}

func runClient(ctx context.Context, devices []usbip.DeviceMatch) error {
	remoteAddress, err := parseServerAddress(clientAddress, usbip.DefaultPort)
	if err != nil {
		return err
	}
	service, err := usbip.NewClientService(ctx, usbip.ClientOptions{
		Logger:        log.StdLogger(),
		ServerAddress: remoteAddress,
		Devices:       devices,
	})
	if err != nil {
		return err
	}
	err = service.Start()
	if err != nil {
		_ = service.Close()
		return err
	}
	if len(devices) == 0 {
		log.Info("importing every device exported by ", remoteAddress)
	} else {
		for _, match := range devices {
			log.Info("importing ", formatDeviceMatch(match))
		}
	}
	<-ctx.Done()
	return service.Close()
}
