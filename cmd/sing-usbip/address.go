//go:build linux || (darwin && cgo) || windows

package main

import (
	"net"
	"strconv"

	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
)

func parseListenAddress(value string, defaultPort uint16) (M.Socksaddr, error) {
	host, port, err := splitHostPort(value, defaultPort)
	if err != nil {
		return M.Socksaddr{}, err
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return M.ParseSocksaddrHostPort(host, port), nil
}

func parseServerAddress(value string, defaultPort uint16) (M.Socksaddr, error) {
	if value == "" {
		return M.Socksaddr{}, E.New("missing --client address")
	}
	host, port, err := splitHostPort(value, defaultPort)
	if err != nil {
		return M.Socksaddr{}, err
	}
	if host == "" {
		return M.Socksaddr{}, E.New("missing host in address: ", value)
	}
	return M.ParseSocksaddrHostPort(host, port), nil
}

func splitHostPort(value string, defaultPort uint16) (string, uint16, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return value, defaultPort, nil
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return "", 0, E.New("invalid port in address: ", value)
	}
	return host, uint16(port), nil
}
