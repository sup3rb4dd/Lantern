//go:build windows

package main

// USB serial ports on Windows, using kernel32 directly so Lantern needs no extra libraries.

import (
	"errors"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procGetCommState    = kernel32.NewProc("GetCommState")
	procSetCommState    = kernel32.NewProc("SetCommState")
	procSetCommTimeouts = kernel32.NewProc("SetCommTimeouts")
	procPurgeComm       = kernel32.NewProc("PurgeComm")
	advapi32            = syscall.NewLazyDLL("advapi32.dll")
	procRegEnumValueW   = advapi32.NewProc("RegEnumValueW")
)

type winDCB struct {
	DCBlength  uint32
	BaudRate   uint32
	Flags      uint32
	wReserved  uint16
	XonLim     uint16
	XoffLim    uint16
	ByteSize   byte
	Parity     byte
	StopBits   byte
	XonChar    byte
	XoffChar   byte
	ErrorChar  byte
	EofChar    byte
	EvtChar    byte
	wReserved1 uint16
}

type winCommTimeouts struct {
	ReadIntervalTimeout         uint32
	ReadTotalTimeoutMultiplier  uint32
	ReadTotalTimeoutConstant    uint32
	WriteTotalTimeoutMultiplier uint32
	WriteTotalTimeoutConstant   uint32
}

type winSerial struct{ h syscall.Handle }

func openSerial(name string) (*winSerial, error) {
	path := name
	if !strings.HasPrefix(path, `\\.\`) {
		path = `\\.\` + name
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return nil, errors.New(name + " is in use by another program (close the Meshtastic app or web flasher)")
		}
		return nil, err
	}
	var d winDCB
	d.DCBlength = uint32(unsafe.Sizeof(d))
	if r, _, e := procGetCommState.Call(uintptr(h), uintptr(unsafe.Pointer(&d))); r == 0 {
		syscall.CloseHandle(h)
		return nil, e
	}
	d.BaudRate = 115200
	d.ByteSize = 8
	d.Parity = 0   // none
	d.StopBits = 0 // one
	// fBinary, DTR on, RTS on (the same lines pyserial and the Meshtastic CLI raise); no flow control.
	d.Flags = 0x0001 | 0x0010 | 0x1000
	if r, _, e := procSetCommState.Call(uintptr(h), uintptr(unsafe.Pointer(&d))); r == 0 {
		syscall.CloseHandle(h)
		return nil, e
	}
	// Return from a read as soon as bytes arrive, or after 200 ms with nothing.
	t := winCommTimeouts{ReadIntervalTimeout: 0xFFFFFFFF, ReadTotalTimeoutMultiplier: 0xFFFFFFFF,
		ReadTotalTimeoutConstant: 200, WriteTotalTimeoutConstant: 5000}
	if r, _, e := procSetCommTimeouts.Call(uintptr(h), uintptr(unsafe.Pointer(&t))); r == 0 {
		syscall.CloseHandle(h)
		return nil, e
	}
	procPurgeComm.Call(uintptr(h), 0x0004|0x0008) // clear stale input/output
	return &winSerial{h: h}, nil
}

func (s *winSerial) Read(b []byte) (int, error) {
	var n uint32
	if err := syscall.ReadFile(s.h, b, &n, nil); err != nil {
		return int(n), err
	}
	if n == 0 {
		return 0, errTimeout
	}
	return int(n), nil
}

func (s *winSerial) Write(b []byte) (int, error) {
	var n uint32
	err := syscall.WriteFile(s.h, b, &n, nil)
	return int(n), err
}

func (s *winSerial) Close() error { return syscall.CloseHandle(s.h) }

// listSerialPorts reads the COM ports Windows knows about from the registry,
// without opening them (opening can reset some boards).
func listSerialPorts() []string {
	var key syscall.Handle
	path, _ := syscall.UTF16PtrFromString(`HARDWARE\DEVICEMAP\SERIALCOMM`)
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, path, 0, syscall.KEY_READ, &key) != nil {
		return nil
	}
	defer syscall.RegCloseKey(key)
	var out []string
	for i := uint32(0); i < 256; i++ {
		name := make([]uint16, 256)
		nameLen := uint32(len(name))
		data := make([]uint16, 64)
		dataLen := uint32(len(data) * 2)
		var typ uint32
		r, _, _ := procRegEnumValueW.Call(uintptr(key), uintptr(i),
			uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&nameLen)), 0,
			uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&dataLen)))
		if r != 0 {
			break
		}
		if v := syscall.UTF16ToString(data); v != "" {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if len(out[a]) != len(out[b]) {
			return len(out[a]) < len(out[b])
		}
		return out[a] < out[b]
	})
	return out
}
