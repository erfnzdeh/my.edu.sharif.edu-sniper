//go:build !windows

package main

// enableANSI is a no op away from Windows, where a terminal understands the
// escape codes without being asked.
func enableANSI(uintptr) bool { return true }
