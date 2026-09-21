//go:build windows

// Package autostart registers the tool to launch at logon via the per-user Run key, which
// needs no elevation and no installer.
package autostart

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const valueName = "MiMoSwitch"

// Enable points the Run key at a verified no-window build, passing `tray` so the restored
// process also shows its icon.
func Enable() error {
	exe, err := silentExecutable()
	if err != nil {
		return err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开 Run 项: %w", err)
	}
	defer key.Close()
	return key.SetStringValue(valueName, fmt.Sprintf(`"%s" tray`, exe))
}

func Disable() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	err = key.DeleteValue(valueName)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

// Enabled reports the current state, and where it would launch from.
func Enabled() (bool, string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false, "", err
	}
	defer key.Close()
	command, _, err := key.GetStringValue(valueName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, command, nil
}
