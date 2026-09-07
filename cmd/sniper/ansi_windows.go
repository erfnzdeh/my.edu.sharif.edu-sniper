package main

import (
	"syscall"
	"unsafe"
)

const enableVirtualTerminalProcessing = 0x0004

// enableANSI turns on virtual terminal processing for the console behind fd.
// Windows Terminal has it on already, the older conhost does not, and without
// it every escape code prints as literal garbage.
func enableANSI(fd uintptr) bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	var mode uint32
	if r, _, _ := getConsoleMode.Call(fd, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ := setConsoleMode.Call(fd, uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
