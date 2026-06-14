//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"context"
	"net"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
)

const backendIDDynamic = "dynamic"

type DeviceTransport interface {
	Submit(request URBRequest) URBResponse
	AbortEndpoint(endpoint uint8) error
}

type DynamicDeviceInfo struct {
	BusID              string
	BusNum             uint32
	DevNum             uint32
	Speed              uint32
	VendorID           uint16
	ProductID          uint16
	BCDDevice          uint16
	DeviceClass        uint8
	DeviceSubClass     uint8
	DeviceProtocol     uint8
	ConfigurationValue uint8
	NumConfigurations  uint8
	Interfaces         []DeviceInterface
	Serial             string
	Product            string
}

func (info DynamicDeviceInfo) toEntry() DeviceEntry {
	var truncated DeviceInfoTruncated
	encodePathField(&truncated.Path, "/dynamic/"+info.BusID)
	copy(truncated.BusID[:len(truncated.BusID)-1], info.BusID)
	truncated.BusNum = info.BusNum
	truncated.DevNum = info.DevNum
	truncated.Speed = info.Speed
	truncated.IDVendor = info.VendorID
	truncated.IDProduct = info.ProductID
	truncated.BCDDevice = info.BCDDevice
	truncated.BDeviceClass = info.DeviceClass
	truncated.BDeviceSubClass = info.DeviceSubClass
	truncated.BDeviceProtocol = info.DeviceProtocol
	truncated.BConfigurationValue = info.ConfigurationValue
	truncated.BNumConfigurations = info.NumConfigurations
	truncated.BNumInterfaces = uint8(len(info.Interfaces))
	return DeviceEntry{
		Info:       truncated,
		Interfaces: info.Interfaces,
		Serial:     info.Serial,
		Product:    info.Product,
	}
}

type DynamicHost struct {
	logger logger.ContextLogger

	access     sync.Mutex
	closed     bool
	nextDevNum uint32
	exports    map[string]*dynamicExport
	lastKeys   map[string]struct{}
	events     chan struct{}
}

func NewDynamicHost(contextLogger logger.ContextLogger) *DynamicHost {
	if contextLogger == nil {
		contextLogger = logger.NOP()
	}
	return &DynamicHost{
		logger:   contextLogger,
		exports:  make(map[string]*dynamicExport),
		lastKeys: make(map[string]struct{}),
		events:   make(chan struct{}, 1),
	}
}

func (h *DynamicHost) Start() error { return nil }

func (h *DynamicHost) Close() error {
	h.access.Lock()
	h.closed = true
	h.exports = make(map[string]*dynamicExport)
	events := h.events
	h.events = nil
	h.access.Unlock()
	if events != nil {
		close(events)
	}
	return nil
}

func (h *DynamicHost) Events() (<-chan struct{}, error) {
	h.access.Lock()
	defer h.access.Unlock()
	return h.events, nil
}

func (h *DynamicHost) Reconcile(isReserved func(busid string) bool) (map[string]Export, []string, error) {
	h.access.Lock()
	defer h.access.Unlock()
	snapshot := make(map[string]Export, len(h.exports))
	current := make(map[string]struct{}, len(h.exports))
	for busid, export := range h.exports {
		snapshot[busid] = export
		current[busid] = struct{}{}
	}
	var released []string
	for busid := range h.lastKeys {
		_, stillPresent := current[busid]
		if !stillPresent {
			released = append(released, busid)
		}
	}
	h.lastKeys = current
	return snapshot, released, nil
}

func (h *DynamicHost) FinishImport(busid string) (bool, error) {
	return false, nil
}

func (h *DynamicHost) AddDevice(info DynamicDeviceInfo, transport DeviceTransport) (string, error) {
	if transport == nil {
		return "", E.New("dynamic device: missing transport")
	}
	busid := info.BusID
	if busid == "" {
		return "", E.New("dynamic device: missing bus id")
	}
	if len(busid) >= 32 {
		return "", E.New("dynamic device: bus id too long: ", busid)
	}
	h.access.Lock()
	defer h.access.Unlock()
	if h.closed {
		return "", E.New("dynamic host closed")
	}
	_, exists := h.exports[busid]
	if exists {
		return "", E.New("dynamic device already provided: ", busid)
	}
	if info.DevNum == 0 {
		h.nextDevNum++
		info.DevNum = h.nextDevNum
	}
	if info.BusNum == 0 {
		info.BusNum = 1
	}
	h.exports[busid] = &dynamicExport{
		busid:     busid,
		entry:     info.toEntry(),
		transport: transport,
		logger:    h.logger,
	}
	h.notifyLocked()
	return busid, nil
}

func (h *DynamicHost) RemoveDevice(busid string) {
	h.access.Lock()
	_, exists := h.exports[busid]
	if exists {
		delete(h.exports, busid)
		h.notifyLocked()
	}
	h.access.Unlock()
}

func (h *DynamicHost) notifyLocked() {
	if h.events == nil {
		return
	}
	select {
	case h.events <- struct{}{}:
	default:
	}
}

type dynamicExport struct {
	busid     string
	entry     DeviceEntry
	transport DeviceTransport
	logger    logger.ContextLogger
}

func (e *dynamicExport) BusID() string {
	return e.busid
}

func (e *dynamicExport) Snapshot(busy bool) ExportSnapshot {
	state := deviceStateAvailable
	if busy {
		state = deviceStateBusy
	}
	return ExportSnapshot{
		Entry:    e.entry,
		Backend:  backendIDDynamic,
		StableID: backendIDDynamic + ":" + e.busid,
		State:    state,
	}
}

func (e *dynamicExport) DeviceInfo() (DeviceInfoTruncated, error) {
	return e.entry.Info, nil
}

func (e *dynamicExport) NewServerDataSession(ctx context.Context, conn net.Conn) (DataSession, error) {
	return newUserspaceURBSession(ctx, e.logger, conn, &dynamicSessionEngine{transport: e.transport}), nil
}

type dynamicSessionEngine struct {
	transport DeviceTransport
}

func (e *dynamicSessionEngine) Submit(request URBRequest) URBResponse {
	return e.transport.Submit(request)
}

func (e *dynamicSessionEngine) AbortEndpoint(endpoint uint8) error {
	return e.transport.AbortEndpoint(endpoint)
}

func (e *dynamicSessionEngine) Close() error {
	return nil
}

func NewDynamicServerService(ctx context.Context, options ServerOptions, host *DynamicHost) (*ServerService, error) {
	if host == nil {
		return nil, E.New("dynamic server: missing host")
	}
	serviceLogger := options.Logger
	if serviceLogger == nil {
		serviceLogger = logger.NOP()
	}
	ctx, cancel := context.WithCancel(ctx)
	return newServerServiceWithHost(ctx, cancel, serviceLogger, options, host), nil
}
