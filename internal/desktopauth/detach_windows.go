//go:build windows

package desktopauth

import "syscall"

// detachAttr keeps the launched app alive after this process exits, without inheriting a
// console. Go's syscall package exposes CreationFlags but not the Win32 constants.
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
}
