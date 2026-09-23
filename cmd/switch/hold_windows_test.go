//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

// holdDir opens dir the way an Explorer window does — for listing, still letting its contents
// change but not letting the node itself go away — which is the exact state an uninstall hits
// when the user double-clicked 卸载 MiMo Switch.exe from inside that very folder.
func holdDir(t *testing.T, dir string) func() {
	t.Helper()
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatalf("占住 %s 失败: %v", dir, err)
	}
	return func() { windows.CloseHandle(h) }
}
