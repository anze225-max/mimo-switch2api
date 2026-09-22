//go:build windows

// Package install makes one downloaded exe behave like an installed program: it anchors
// itself in %LOCALAPPDATA%, registers the logon launch and Start Menu entries, and can take
// all of it back out again. There is no installer to run.
package install

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	dirName = "MiMoSwitch"
	exeName = "MiMoSwitch.exe"

	appLink       = "MiMo Switch.lnk"
	uninstallLink = "卸载 MiMo Switch.lnk"
)

// Target is where the app wants to live: a stable per-user path that survives the download
// folder being cleaned out.
func Target() (dir string, exe string, err error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", "", fmt.Errorf("找不到 %%LOCALAPPDATA%%，无法自我安置")
	}
	dir = filepath.Join(base, dirName)
	return dir, filepath.Join(dir, exeName), err
}

// IsDistributable reports whether an executable is the GUI build users install, as opposed
// to the console build used while developing.
func IsDistributable(exe string) bool {
	return strings.EqualFold(filepath.Base(exe), exeName)
}

// Relocate copies the running distributable into Target and returns its new path, or "" when
// nothing needs doing. Only the GUI build is named MiMoSwitch.exe, so the console build used
// while developing never moves itself.
func Relocate() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if !IsDistributable(self) {
		return "", nil
	}
	dir, exe, err := Target()
	if err != nil {
		return "", err
	}
	if strings.EqualFold(self, exe) {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Copy then rename, so an interrupted write can never leave a half-written exe in place.
	tmp := exe + ".new"
	src, err := os.Open(self)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return exe, nil
}

// StartMenu puts the app and its uninstaller entry in the user's Programs folder, which is
// writable without elevation.
func StartMenu() error {
	programs, err := programsDir()
	if err != nil {
		return err
	}
	dir, exe, err := Target()
	if err != nil {
		return err
	}
	if err := link(filepath.Join(programs, appLink), exe, "", dir, "MiMo 免费额度本地端点"); err != nil {
		return err
	}
	return link(filepath.Join(programs, uninstallLink), exe, "uninstall", dir, "一键卸载 MiMo Switch")
}

// RemoveStartMenu deletes both entries; missing files are not an error.
func RemoveStartMenu() error {
	programs, err := programsDir()
	if err != nil {
		return err
	}
	var firstErr error
	for _, name := range []string{appLink, uninstallLink} {
		if err := os.Remove(filepath.Join(programs, name)); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SelfDelete removes dir once this process has exited. Windows will not let a running exe
// delete its own image, so a detached command waits a moment and does it afterwards.
func SelfDelete(dir string) error {
	script := fmt.Sprintf("ping -n 4 127.0.0.1 >nul & rmdir /s /q %q", dir)
	cmd := exec.Command("cmd", "/c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
	return cmd.Start()
}

// RunningFromTarget reports whether this process lives inside dir, which is the only case
// where a self-delete makes sense.
func RunningFromTarget(dir string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	prefix := strings.TrimRight(dir, `\/`) + string(os.PathSeparator)
	return strings.HasPrefix(strings.ToLower(self), strings.ToLower(prefix))
}

func programsDir() (string, error) {
	base := os.Getenv("APPDATA")
	if base == "" {
		return "", fmt.Errorf("找不到 %%APPDATA%%，无法定位开始菜单")
	}
	return filepath.Join(base, "Microsoft", "Windows", "Start Menu", "Programs"), nil
}

// link writes a .lnk through the shell COM object, which is the only way to get a normal
// Start Menu entry without shipping an installer.
func link(path, target, args, workDir, description string) error {
	script := fmt.Sprintf(
		"$s = New-Object -ComObject WScript.Shell; "+
			"$l = $s.CreateShortcut(%s); $l.TargetPath = %s; $l.Arguments = %s; "+
			"$l.WorkingDirectory = %s; $l.Description = %s; $l.Save()",
		psQuote(path), psQuote(target), psQuote(args), psQuote(workDir), psQuote(description))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("创建快捷方式失败: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// psQuote renders a Go string as a single-quoted PowerShell literal, doubling any quote.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)
