//go:build !linux && !(darwin && cgo) && !windows

package main

import "github.com/sagernet/sing-box/log"

func main() {
	log.Fatal("sing-usbip is only supported on Linux, Windows, and macOS with CGO")
}
