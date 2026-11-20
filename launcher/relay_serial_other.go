//go:build !windows && !linux

package main

import (
	"errors"
	"io"
)

func openSerial(name string) (io.ReadWriteCloser, error) {
	return nil, errors.New("USB serial isn't supported on this system yet; connect the node over WiFi (tcp:ADDRESS)")
}

func listSerialPorts() []string { return nil }
