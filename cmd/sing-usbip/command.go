//go:build linux || (darwin && cgo) || windows

package main

import (
	"context"
	"os"
	"time"

	"github.com/sagernet/sing-box/log"

	"github.com/spf13/cobra"
)

var (
	serverAddress string
	clientAddress string
	logLevel      string
)

var mainCommand = &cobra.Command{
	Use:   "sing-usbip",
	Short: "USB/IP server and client",
	Long: "USB/IP server and client.\n\n" +
		"Run as a server (--server) to export local USB devices, or as a client\n" +
		"(--client) to import devices exported by a USB/IP server.\n\n" +
		deviceMatchSyntax,
	PersistentPreRun: preRun,
	SilenceUsage:     true,
	SilenceErrors:    true,
}

func init() {
	mainCommand.PersistentFlags().StringVarP(&serverAddress, "server", "s", "", "export devices, listening at this address (host[:port], e.g. :3240)")
	mainCommand.PersistentFlags().StringVarP(&clientAddress, "client", "c", "", "import devices from this USB/IP server (host[:port])")
	mainCommand.PersistentFlags().StringVarP(&logLevel, "log-level", "", "info", "log level: trace, debug, info, warn or error")
	mainCommand.MarkFlagsMutuallyExclusive("server", "client")
}

func preRun(_ *cobra.Command, _ []string) {
	level, err := log.ParseLevel(logLevel)
	if err != nil {
		log.Fatal(err)
	}
	factory := log.NewDefaultFactory(context.Background(), log.Formatter{BaseTime: time.Now()}, os.Stderr, "", nil, false)
	factory.SetLevel(level)
	log.SetStdLogger(factory.Logger())
}
