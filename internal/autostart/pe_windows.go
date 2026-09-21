//go:build windows

package autostart

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// subsystemWindowsGUI is the PE subsystem value that launches without a console window.
const subsystemWindowsGUI = 2

// silentExecutable picks the binary to register for autostart: whichever one is actually a
// GUI-subsystem build. Registering the console build would flash a window at logon, so a
// silent start is verified against the PE header rather than assumed from the file name.
func silentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)

	candidates := []string{
		filepath.Join(dir, "MiMoSwitch.exe"),
		exe,
	}
	for _, candidate := range candidates {
		sub, err := peSubsystem(candidate)
		if err != nil {
			continue
		}
		if sub == subsystemWindowsGUI {
			return candidate, nil
		}
	}
	return "", errors.New("找不到无窗口版本的可执行文件（请用 -ldflags \"-H=windowsgui\" 构建的 MiMoSwitch.exe），拒绝注册会弹控制台的自启项")
}

// peSubsystem reads the PE Subsystem field out of an executable's header.
func peSubsystem(path string) (uint16, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	head := make([]byte, 64)
	if _, err := f.ReadAt(head, 0); err != nil {
		return 0, fmt.Errorf("读取文件头: %w", err)
	}
	if binary.LittleEndian.Uint16(head[0:2]) != 0x5A4D { // "MZ"
		return 0, errors.New("不是 PE 文件")
	}
	peOff := int(binary.LittleEndian.Uint32(head[60:64]))

	// COFF header (20 bytes) follows the "PE\0\0" signature; the optional header follows,
	// and Subsystem sits at offset 68 within it.
	coff := make([]byte, 24)
	if _, err := f.ReadAt(coff, int64(peOff)); err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint32(coff[0:4]) != 0x00004550 { // "PE\0\0"
		return 0, errors.New("PE 签名不符")
	}
	optOff := peOff + 4 + 20
	buf := make([]byte, 70)
	if _, err := f.ReadAt(buf, int64(optOff)); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(buf[68:70]), nil
}
