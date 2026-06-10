//go:build linux || (darwin && cgo) || windows

package main

import (
	"fmt"
	"strconv"
	"strings"

	usbip "github.com/sagernet/sing-usbip"
	E "github.com/sagernet/sing/common/exceptions"
)

const deviceMatchSyntax = "Device match syntax (comma-separated criteria, all must match):\n" +
	"  busid=1-1.2  vendor_id=0x0bda  product_id=0x8153  serial=001000001\n" +
	"A bare value is a busid; vvvv:pppp is shorthand for vendor_id:product_id."

func parseDeviceMatches(values []string) ([]usbip.DeviceMatch, error) {
	matches := make([]usbip.DeviceMatch, 0, len(values))
	for _, value := range values {
		match, err := parseDeviceMatch(value)
		if err != nil {
			return nil, err
		}
		matches = append(matches, match)
	}
	return matches, nil
}

func parseDeviceMatch(value string) (usbip.DeviceMatch, error) {
	var match usbip.DeviceMatch
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		key, keyValue, hasKey := strings.Cut(part, "=")
		switch {
		case hasKey:
			err := setDeviceMatchField(&match, key, keyValue)
			if err != nil {
				return usbip.DeviceMatch{}, err
			}
		case strings.Contains(part, ":"):
			vendorText, productText, _ := strings.Cut(part, ":")
			vendorID, err := parseHexID(vendorText)
			if err != nil {
				return usbip.DeviceMatch{}, E.Cause(err, "vendor id in ", part)
			}
			productID, err := parseHexID(productText)
			if err != nil {
				return usbip.DeviceMatch{}, E.Cause(err, "product id in ", part)
			}
			match.VendorID = vendorID
			match.ProductID = productID
		case part != "":
			match.BusID = part
		default:
			return usbip.DeviceMatch{}, E.New("empty component in device match: ", value)
		}
	}
	if match.IsZero() {
		return usbip.DeviceMatch{}, E.New("empty device match: ", value)
	}
	return match, nil
}

func setDeviceMatchField(match *usbip.DeviceMatch, key string, value string) error {
	switch key {
	case "busid":
		match.BusID = value
	case "vendor_id":
		vendorID, err := parseHexID(value)
		if err != nil {
			return E.Cause(err, "vendor_id")
		}
		match.VendorID = vendorID
	case "product_id":
		productID, err := parseHexID(value)
		if err != nil {
			return E.Cause(err, "product_id")
		}
		match.ProductID = productID
	case "serial":
		match.Serial = value
	default:
		return E.New("unknown device match key: ", key)
	}
	return nil
}

func parseHexID(value string) (uint16, error) {
	parsed, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(value), "0x"), 16, 16)
	if err != nil {
		return 0, E.New("invalid hex id: ", value)
	}
	if parsed == 0 {
		return 0, E.New("id 0x0000 cannot be matched")
	}
	return uint16(parsed), nil
}

func formatDeviceMatch(match usbip.DeviceMatch) string {
	var parts []string
	if match.BusID != "" {
		parts = append(parts, "busid="+match.BusID)
	}
	if match.VendorID != 0 {
		parts = append(parts, fmt.Sprintf("vendor_id=0x%04x", match.VendorID))
	}
	if match.ProductID != 0 {
		parts = append(parts, fmt.Sprintf("product_id=0x%04x", match.ProductID))
	}
	if match.Serial != "" {
		parts = append(parts, "serial="+match.Serial)
	}
	return strings.Join(parts, ",")
}
