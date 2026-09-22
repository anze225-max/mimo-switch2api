//go:build windows

package tray

import "syscall"

// StartedWithoutConsole reports whether the process has no console to write to, i.e. it was
// launched by double-clicking the GUI build rather than from a terminal. Diagnostics then
// have to be shown some other way.
func StartedWithoutConsole() bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	hwnd, _, _ := kernel32.NewProc("GetConsoleWindow").Call()
	return hwnd == 0
}
