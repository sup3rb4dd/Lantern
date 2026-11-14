//go:build !windows

package main

import (
	"os/exec"
	"runtime"
	"syscall"
)

func hideWindow(cmd *exec.Cmd)  {}
func attachToJob(cmd *exec.Cmd) {}
func setConsoleTitle(t string)  {}

func openBrowser(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	exec.Command(name, url).Start()
}

func freeSpace(path string) (free, total uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize)
}
