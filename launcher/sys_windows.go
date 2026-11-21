//go:build windows

package main

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procGetDiskFreeSpaceExW      = kernel32.NewProc("GetDiskFreeSpaceExW")
	procSetConsoleTitleW         = kernel32.NewProc("SetConsoleTitleW")
	procGlobalMemoryStatusEx     = kernel32.NewProc("GlobalMemoryStatusEx")
)

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

const (
	jobObjectExtendedLimitInformation = 9
	jobLimitKillOnJobClose            = 0x2000
	createNoWindow                    = 0x08000000
)

var (
	jobOnce   sync.Once
	jobHandle uintptr
)

// getJob returns a job object that kills its child processes when Lantern exits,
// so kiwix-serve never keeps running after the window is closed.
func getJob() uintptr {
	jobOnce.Do(func() {
		h, _, _ := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			return
		}
		var info jobExtendedLimit
		info.Basic.LimitFlags = jobLimitKillOnJobClose
		r, _, _ := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if r == 0 {
			syscall.CloseHandle(syscall.Handle(h))
			return
		}
		jobHandle = h
	})
	return jobHandle
}

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func attachToJob(cmd *exec.Cmd) {
	job := getJob()
	if job == 0 || cmd.Process == nil {
		return
	}
	const access = 0x0001 | 0x0100 // PROCESS_TERMINATE | PROCESS_SET_QUOTA
	ph, err := syscall.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	procAssignProcessToJobObject.Call(job, uintptr(ph))
	syscall.CloseHandle(ph)
}

func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.Start()
}

func freeSpace(path string) (free, total uint64) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}
	var avail, tot, totFree uint64
	r, _, _ := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&tot)), uintptr(unsafe.Pointer(&totFree)))
	if r == 0 {
		return 0, 0
	}
	return avail, tot
}

func setConsoleTitle(t string) {
	p, err := syscall.UTF16PtrFromString(t)
	if err == nil {
		procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
	}
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// memoryInfo returns installed and currently free RAM in bytes (0, 0 if unknown).
func memoryInfo() (total, avail uint64) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, 0
	}
	return m.TotalPhys, m.AvailPhys
}
