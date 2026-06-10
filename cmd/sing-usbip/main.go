//go:build linux || (darwin && cgo) || windows

package main

import "github.com/sagernet/sing-box/log"

func main() {
	err := mainCommand.Execute()
	if err != nil {
		log.Fatal(err)
	}
}
