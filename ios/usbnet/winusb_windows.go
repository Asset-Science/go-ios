//go:build windows

package usbnet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"github.com/danielpaulus/go-ios/ios/golog"
	"golang.org/x/sys/windows"
)

// NcmInterfaceGUID is the DeviceInterfaceGUIDs value the WinUSB driver package for the phone's NCM
// function registers (DESK-4038 desk4038_ncm_winusb.inf). The transport opens the phone by it.
var NcmInterfaceGUID = windows.GUID{Data1: 0x4fcf489f, Data2: 0x7f03, Data3: 0x4dc9,
	Data4: [8]byte{0xa4, 0x30, 0xe3, 0x00, 0x02, 0x1c, 0x18, 0x51}}

var (
	winusb   = windows.NewLazySystemDLL("winusb.dll")
	cfgmgr32 = windows.NewLazySystemDLL("cfgmgr32.dll")

	procInitialize             = winusb.NewProc("WinUsb_Initialize")
	procFree                   = winusb.NewProc("WinUsb_Free")
	procGetAssociatedInterface = winusb.NewProc("WinUsb_GetAssociatedInterface")
	procQueryInterfaceSettings = winusb.NewProc("WinUsb_QueryInterfaceSettings")
	procQueryPipe              = winusb.NewProc("WinUsb_QueryPipe")
	procSetCurrentAltSetting   = winusb.NewProc("WinUsb_SetCurrentAlternateSetting")
	procControlTransfer        = winusb.NewProc("WinUsb_ControlTransfer")
	procReadPipe               = winusb.NewProc("WinUsb_ReadPipe")
	procWritePipe              = winusb.NewProc("WinUsb_WritePipe")
	procSetPipePolicy          = winusb.NewProc("WinUsb_SetPipePolicy")
	procGetOverlappedResult    = winusb.NewProc("WinUsb_GetOverlappedResult")

	procGetInterfaceListSize = cfgmgr32.NewProc("CM_Get_Device_Interface_List_SizeW")
	procGetInterfaceList     = cfgmgr32.NewProc("CM_Get_Device_Interface_ListW")
	procGetInterfaceProperty = cfgmgr32.NewProc("CM_Get_Device_Interface_PropertyW")
	procLocateDevNode        = cfgmgr32.NewProc("CM_Locate_DevNodeW")
	procGetParent            = cfgmgr32.NewProc("CM_Get_Parent")
	procGetDeviceID          = cfgmgr32.NewProc("CM_Get_Device_IDW")
)

const (
	pipeTransferTimeout     = 0x03 // WinUsb_SetPipePolicy PIPE_TRANSFER_TIMEOUT
	usbdPipeTypeBulk        = 2
	usbdPipeTypeInterrupt   = 3
	readTimeoutMillis       = 100
	writeTimeoutMillis      = 1000
	cmGetListPresent        = 0
	crSuccess               = 0
	maxDeviceIDLength       = 200
	errorSemTimeout         = windows.Errno(121) // ERROR_SEM_TIMEOUT: a pipe timeout, not a failure
	devpropTypeString       = 0x12
	usbInterfaceDescLength  = 9
	winusbPipeInfoSizeBytes = 12
)

// DEVPKEY_Device_InstanceId
var devpkeyInstanceID = struct {
	fmtid windows.GUID
	pid   uint32
}{windows.GUID{Data1: 0x78c34fc8, Data2: 0x104a, Data3: 0x4aca, Data4: [8]byte{0x9e, 0xa4, 0x52, 0x4d, 0x52, 0x99, 0x6e, 0x57}}, 256}

type pipeInformation struct {
	PipeType          uint32
	PipeID            uint8
	_                 uint8
	MaximumPacketSize uint16
	Interval          uint8
	_                 [3]uint8
}

// WinUSBTransport is a phone's NCM function opened through the inbox WinUSB driver.
//
// Reads and writes run concurrently (the link reads in one goroutine and writes from others), so
// they use overlapped I/O: two synchronous calls on the one interface handle starved the writes
// on the bench (every write hit its timeout while a read was pending). Each direction owns one
// OVERLAPPED, kept in this heap-allocated struct because the driver holds its address until the
// transfer completes — a stack copy could move under it. Reads come from one goroutine and the
// link serializes writes, so one per direction is enough.
type WinUSBTransport struct {
	file    windows.Handle
	control uintptr // WINUSB_INTERFACE_HANDLE of the communication interface
	data    uintptr // ... of the associated data interface
	bulkIn  uint8
	bulkOut uint8
	params  NtbParameters

	readOv  windows.Overlapped
	writeOv windows.Overlapped

	// The phone's notifications are read and dropped for the life of the link: the bench showed
	// its bulk OUT pipe NAKs every write (each one times out) unless the host keeps its interrupt
	// pipe drained, as the C++ probe did and this transport at first did not.
	notifyPipe uint8
	notifyBuf  []byte
	notifyOv   windows.Overlapped
	stop       chan struct{}
	wg         sync.WaitGroup
}

// OpenTransport finds the NCM function of the phone with this UDID (go-ios form; the USB serial is
// the UDID without its dash) and brings it up the way Linux's cdc_ncm does. The function must be
// in network mode with the DESK-4038 WinUSB package bound to it.
func OpenTransport(udid string) (*WinUSBTransport, error) {
	paths, err := interfacePaths(&NcmInterfaceGUID)
	if err != nil {
		return nil, err
	}
	want := strings.ToUpper(strings.ReplaceAll(udid, "-", ""))
	for _, p := range paths {
		serial, err := parentSerial(p)
		if err != nil || !strings.EqualFold(serial, want) {
			continue
		}
		return openPath(p)
	}
	return nil, fmt.Errorf("usbnet: no NCM function for %s (is the phone in network mode with the WinUSB package bound?)", udid)
}

func openPath(path string) (*WinUSBTransport, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	file, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("usbnet: open NCM function: %w", err)
	}
	t := &WinUSBTransport{file: file}
	for _, ov := range []*windows.Overlapped{&t.readOv, &t.writeOv, &t.notifyOv} {
		ev, err := windows.CreateEvent(nil, 1, 0, nil) // manual reset, as GetOverlappedResult expects
		if err != nil {
			t.Close()
			return nil, fmt.Errorf("usbnet: CreateEvent: %w", err)
		}
		ov.HEvent = ev
	}
	if r, _, e := procInitialize.Call(uintptr(file), uintptr(unsafe.Pointer(&t.control))); r == 0 {
		t.Close()
		return nil, fmt.Errorf("usbnet: WinUsb_Initialize: %w", e)
	}
	if err := t.bringUp(); err != nil {
		t.Close()
		return nil, err
	}
	t.notifyBuf = make([]byte, 64)
	t.stop = make(chan struct{})
	t.wg.Add(1)
	go t.drainNotifications()
	return t, nil
}

func (t *WinUSBTransport) bringUp() error {
	var desc [usbInterfaceDescLength]byte
	if r, _, e := procQueryInterfaceSettings.Call(t.control, 0, uintptr(unsafe.Pointer(&desc[0]))); r == 0 {
		return fmt.Errorf("usbnet: query control interface: %w", e)
	}
	controlIf := uint16(desc[2])
	for i := 0; i < int(desc[4]); i++ {
		var pipe pipeInformation
		if r, _, _ := procQueryPipe.Call(t.control, 0, uintptr(i), uintptr(unsafe.Pointer(&pipe))); r != 0 && pipe.PipeType == usbdPipeTypeInterrupt && pipe.PipeID&0x80 != 0 {
			t.notifyPipe = pipe.PipeID
		}
	}
	if t.notifyPipe == 0 {
		return errors.New("usbnet: communication interface has no notification pipe")
	}
	if err := t.setTimeout(t.control, t.notifyPipe, readTimeoutMillis); err != nil {
		return err
	}
	if r, _, e := procGetAssociatedInterface.Call(t.control, 0, uintptr(unsafe.Pointer(&t.data))); r == 0 {
		return fmt.Errorf("usbnet: no associated data interface (is usbccgp grouping on?): %w", e)
	}

	var reply [ntbParametersLength]byte
	if _, err := t.classRequest(requestTypeClassInterfaceIn, requestGetNtbParameters, 0, controlIf, reply[:]); err != nil {
		return fmt.Errorf("usbnet: GET_NTB_PARAMETERS: %w", err)
	}
	params, err := ParseNtbParameters(reply[:])
	if err != nil {
		return err
	}
	t.params = params
	var inSize [4]byte
	binary.LittleEndian.PutUint32(inSize[:], params.NtbInMaxSize)
	if _, err := t.classRequest(requestTypeClassInterfaceOut, requestSetNtbInputSize, 0, controlIf, inSize[:]); err != nil {
		return fmt.Errorf("usbnet: SET_NTB_INPUT_SIZE: %w", err)
	}
	for _, alt := range []uintptr{0, 1} {
		if r, _, e := procSetCurrentAltSetting.Call(t.data, alt); r == 0 {
			return fmt.Errorf("usbnet: data interface alt %d: %w", alt, e)
		}
	}
	if _, err := t.classRequest(requestTypeClassInterfaceOut, requestSetEthernetPacketFilter, defaultPacketFilter, controlIf, nil); err != nil {
		return fmt.Errorf("usbnet: SET_ETHERNET_PACKET_FILTER: %w", err)
	}

	if r, _, e := procQueryInterfaceSettings.Call(t.data, 1, uintptr(unsafe.Pointer(&desc[0]))); r == 0 {
		return fmt.Errorf("usbnet: query data interface: %w", e)
	}
	for i := 0; i < int(desc[4]); i++ {
		var pipe pipeInformation
		if r, _, _ := procQueryPipe.Call(t.data, 1, uintptr(i), uintptr(unsafe.Pointer(&pipe))); r != 0 && pipe.PipeType == usbdPipeTypeBulk {
			if pipe.PipeID&0x80 != 0 {
				t.bulkIn = pipe.PipeID
			} else {
				t.bulkOut = pipe.PipeID
			}
		}
	}
	if t.bulkIn == 0 || t.bulkOut == 0 {
		return errors.New("usbnet: data interface has no bulk IN/OUT pair")
	}
	if err := t.setTimeout(t.data, t.bulkIn, readTimeoutMillis); err != nil {
		return err
	}
	return t.setTimeout(t.data, t.bulkOut, writeTimeoutMillis)
}

func (t *WinUSBTransport) setTimeout(iface uintptr, pipe uint8, millis uint32) error {
	if r, _, e := procSetPipePolicy.Call(iface, uintptr(pipe), pipeTransferTimeout, 4, uintptr(unsafe.Pointer(&millis))); r == 0 {
		return fmt.Errorf("usbnet: pipe timeout: %w", e)
	}
	return nil
}

// classRequest sends one class request on the communication interface. The WINUSB_SETUP_PACKET is an
// 8-byte struct passed by value, which the x64 ABI passes in one register.
func (t *WinUSBTransport) classRequest(requestType, request uint8, value, index uint16, buf []byte) (int, error) {
	setup := uint64(requestType) | uint64(request)<<8 | uint64(value)<<16 | uint64(index)<<32 | uint64(len(buf))<<48
	var ptr uintptr
	if len(buf) > 0 {
		ptr = uintptr(unsafe.Pointer(&buf[0]))
	}
	var n uint32
	if r, _, e := procControlTransfer.Call(t.control, uintptr(setup), ptr, uintptr(len(buf)), uintptr(unsafe.Pointer(&n)), 0); r == 0 {
		return 0, e
	}
	return int(n), nil
}

// transfer starts one overlapped pipe transfer and waits for it. The pipe's PIPE_TRANSFER_TIMEOUT
// still bounds it: a timed-out transfer completes with ERROR_SEM_TIMEOUT. buf must be heap memory
// (both callers pass slices the link allocated), since the driver uses it after the call returns.
func (t *WinUSBTransport) transfer(iface uintptr, proc *windows.LazyProc, pipe uint8, buf []byte, ov *windows.Overlapped) (uint32, error) {
	if err := windows.ResetEvent(ov.HEvent); err != nil {
		return 0, err
	}
	r, _, e := proc.Call(iface, uintptr(pipe), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, uintptr(unsafe.Pointer(ov)))
	if r == 0 && !errors.Is(e, windows.ERROR_IO_PENDING) {
		return 0, e
	}
	var n uint32
	if r, _, e := procGetOverlappedResult.Call(iface, uintptr(unsafe.Pointer(ov)), uintptr(unsafe.Pointer(&n)), 1); r == 0 {
		return 0, e
	}
	return n, nil
}

// ReadNtb implements Transport; a pipe timeout is (0, nil).
func (t *WinUSBTransport) ReadNtb(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	n, err := t.transfer(t.data, procReadPipe, t.bulkIn, buf, &t.readOv)
	if err != nil {
		if errors.Is(err, errorSemTimeout) {
			return 0, nil
		}
		return 0, fmt.Errorf("usbnet: read: %w", err)
	}
	return int(n), nil
}

// WriteNtb implements Transport.
func (t *WinUSBTransport) WriteNtb(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if _, err := t.transfer(t.data, procWritePipe, t.bulkOut, b, &t.writeOv); err != nil {
		return fmt.Errorf("usbnet: write: %w", err)
	}
	return nil
}

// drainNotifications keeps the interrupt pipe empty until Close; a failure other than a
// timeout means the device went away, which the data path reports on its own.
func (t *WinUSBTransport) drainNotifications() {
	defer t.wg.Done()
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		n, err := t.transfer(t.control, procReadPipe, t.notifyPipe, t.notifyBuf, &t.notifyOv)
		if err != nil {
			if errors.Is(err, errorSemTimeout) {
				continue
			}
			golog.Debug("usbnet notification read ended", "module", logModule, "error", err)
			return
		}
		golog.Trace("usbnet notification", "module", logModule, "bytes", fmt.Sprintf("%x", t.notifyBuf[:n]))
	}
}

// Params implements Transport.
func (t *WinUSBTransport) Params() NtbParameters { return t.params }

// Close releases the WinUSB handles and the device file, data interface first.
func (t *WinUSBTransport) Close() {
	if t.stop != nil {
		close(t.stop) // the drain loop ends within one read timeout
		t.wg.Wait()
		t.stop = nil
	}
	if t.data != 0 {
		_, _, _ = procFree.Call(t.data)
		t.data = 0
	}
	if t.control != 0 {
		_, _, _ = procFree.Call(t.control)
		t.control = 0
	}
	if t.file != 0 {
		_ = windows.CloseHandle(t.file)
		t.file = 0
	}
	for _, ov := range []*windows.Overlapped{&t.readOv, &t.writeOv, &t.notifyOv} {
		if ov.HEvent != 0 {
			_ = windows.CloseHandle(ov.HEvent)
			ov.HEvent = 0
		}
	}
}

// interfacePaths lists the present device interfaces of a class.
func interfacePaths(guid *windows.GUID) ([]string, error) {
	var size uint32
	if r, _, _ := procGetInterfaceListSize.Call(uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(guid)), 0, cmGetListPresent); r != crSuccess {
		return nil, fmt.Errorf("usbnet: CM_Get_Device_Interface_List_Size: CONFIGRET %d", r)
	}
	if size <= 1 {
		return nil, nil
	}
	buf := make([]uint16, size)
	if r, _, _ := procGetInterfaceList.Call(uintptr(unsafe.Pointer(guid)), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(size), cmGetListPresent); r != crSuccess {
		return nil, fmt.Errorf("usbnet: CM_Get_Device_Interface_List: CONFIGRET %d", r)
	}
	var out []string
	for start := 0; start < len(buf) && buf[start] != 0; {
		end := start
		for end < len(buf) && buf[end] != 0 {
			end++
		}
		out = append(out, windows.UTF16ToString(buf[start:end]))
		start = end + 1
	}
	return out, nil
}

// parentSerial returns the USB serial (the instance-id tail) of the composite device a function
// interface belongs to.
func parentSerial(interfacePath string) (string, error) {
	p, err := windows.UTF16PtrFromString(interfacePath)
	if err != nil {
		return "", err
	}
	var propType uint32
	id := make([]uint16, maxDeviceIDLength)
	size := uint32(len(id) * 2)
	if r, _, _ := procGetInterfaceProperty.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&devpkeyInstanceID)),
		uintptr(unsafe.Pointer(&propType)), uintptr(unsafe.Pointer(&id[0])), uintptr(unsafe.Pointer(&size)), 0); r != crSuccess || propType != devpropTypeString {
		return "", fmt.Errorf("usbnet: interface instance id: CONFIGRET %d", r)
	}
	var child, parent uint32
	if r, _, _ := procLocateDevNode.Call(uintptr(unsafe.Pointer(&child)), uintptr(unsafe.Pointer(&id[0])), 0); r != crSuccess {
		return "", fmt.Errorf("usbnet: CM_Locate_DevNode: CONFIGRET %d", r)
	}
	if r, _, _ := procGetParent.Call(uintptr(unsafe.Pointer(&parent)), uintptr(child), 0); r != crSuccess {
		return "", fmt.Errorf("usbnet: CM_Get_Parent: CONFIGRET %d", r)
	}
	pid := make([]uint16, maxDeviceIDLength)
	if r, _, _ := procGetDeviceID.Call(uintptr(parent), uintptr(unsafe.Pointer(&pid[0])), uintptr(len(pid)), 0); r != crSuccess {
		return "", fmt.Errorf("usbnet: CM_Get_Device_ID: CONFIGRET %d", r)
	}
	parentID := windows.UTF16ToString(pid)
	return parentID[strings.LastIndex(parentID, `\`)+1:], nil
}
