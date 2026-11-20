//go:build linux

package main

// USB serial ports on Linux (for a Raspberry Pi or other Linux gateway later).

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

type linuxSerial struct{ f *os.File }

func openSerial(name string) (*linuxSerial, error) {
	if !strings.HasPrefix(name, "/") {
		name = "/dev/" + name
	}
	f, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	var t syscall.Termios
	fd := f.Fd()
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		f.Close()
		return nil, e
	}
	// raw mode, 8N1, 115200, reads return after 0.2 s with no data
	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag = syscall.CS8 | syscall.CREAD | syscall.CLOCAL | syscall.B115200
	t.Ispeed = syscall.B115200
	t.Ospeed = syscall.B115200
	t.Cc[syscall.VMIN] = 0
	t.Cc[syscall.VTIME] = 2
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		f.Close()
		return nil, e
	}
	return &linuxSerial{f: f}, nil
}

func (s *linuxSerial) Read(b []byte) (int, error) {
	n, err := s.f.Read(b)
	if n == 0 && (err == nil || errors.Is(err, io.EOF)) {
		return 0, errTimeout
	}
	return n, err
}

func (s *linuxSerial) Write(b []byte) (int, error) { return s.f.Write(b) }
func (s *linuxSerial) Close() error                { return s.f.Close() }

func listSerialPorts() []string {
	var out []string
	for _, pat := range []string{"/dev/ttyACM*", "/dev/ttyUSB*"} {
		m, _ := filepath.Glob(pat)
		out = append(out, m...)
	}
	sort.Strings(out)
	return out
}
