//go:build windows

// Package notify shows the user a message when there is no console to write to, which is
// the normal case for the distributed GUI build launched by double-click.
package notify

import (
	"syscall"
	"unsafe"
)

func messageBox(title, text string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	// MB_OK | MB_ICONWARNING | MB_TOPMOST, so a first-run failure is not hidden behind the
	// browser window the login flow opened. No owner window: this process has none.
	const flags = 0x00000030 | 0x00040000
	user32.NewProc("MessageBoxW").Call(
		0,
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(text))),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(title))),
		uintptr(flags),
	)
}

// Error reports a failed start-up to someone who cannot see stderr.
func Error(title string, err error) {
	if err == nil {
		return
	}
	messageBox(title, err.Error())
}
