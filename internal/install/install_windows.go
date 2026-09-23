//go:build windows

// Package install makes one downloaded exe behave like an installed program: it asks for a
// folder the first time, anchors itself there, registers the logon launch and Start Menu
// entries, and can take all of it back out again. There is no installer to run.
package install

import (
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

//go:embed readme.txt
var readmeText string

const (
	dirName = "MiMoSwitch"
	exeName = "MiMoSwitch.exe"

	// uninstallerName is a byte-identical copy of the main exe that learns its job from its
	// own file name, so the install folder holds a real double-clickable uninstaller.
	uninstallerName = "卸载 MiMo Switch.exe"

	// readmeName is the 使用说明 the install drops beside the exe, so the download a user
	// gets can stay a bare exe and the documentation lives where the app lives.
	readmeName = "README.txt"

	// stagedPrefix names the temp copy an uninstall continues from, so one pattern serves both
	// recognising it and sweeping the ones an abnormal exit left behind.
	stagedPrefix = exeName + ".uninstall-"

	appLink       = "MiMo Switch.lnk"
	uninstallLink = "卸载 MiMo Switch.lnk"

	// createNoWindow suppresses a helper's console without marking its primary window hidden.
	// STARTF_USESHOWWINDOW/SW_HIDE is inherited by any dialog the helper opens, and a dialog
	// that obeys it runs invisibly while the caller blocks waiting for a click.
	createNoWindow = 0x08000000
)

// Target resolves where the app lives. recorded is the folder picked at install time; empty
// falls back to the per-user default, which is also where every copy predating the picker
// already sat.
func Target(recorded string) (dir, exe string, err error) {
	if recorded != "" {
		// A relative record would resolve against whatever the current directory happens to
		// be, and uninstall deletes by this path.
		if !filepath.IsAbs(recorded) {
			return "", "", fmt.Errorf("安装目录必须是绝对路径，当前为 %q", recorded)
		}
		return recorded, filepath.Join(recorded, exeName), nil
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", "", fmt.Errorf("找不到 %%LOCALAPPDATA%%，无法自我安置")
	}
	dir = filepath.Join(base, dirName)
	return dir, filepath.Join(dir, exeName), nil
}

// IsDistributable reports whether an executable is the GUI build users install, as opposed
// to the console build used while developing.
func IsDistributable(exe string) bool {
	return strings.EqualFold(filepath.Base(exe), exeName)
}

// IsUninstaller reports whether an executable is the copy that only exists to uninstall.
func IsUninstaller(exe string) bool {
	return strings.EqualFold(filepath.Base(exe), uninstallerName)
}

// LooksInstalled reports whether dir actually holds one of our executables. Uninstall deletes
// a whole folder, so it refuses to act on a directory that was never an install target.
func LooksInstalled(dir string) bool {
	for _, name := range []string{exeName, uninstallerName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// HasUninstaller reports whether dir is a real install folder rather than a download that
// happens to hold a copy of the exe: only an installed folder gets the uninstaller copy.
func HasUninstaller(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, uninstallerName))
	return err == nil
}

// OnlyOurs reports whether dir contains nothing but this app's own files. The folder picker
// lets the user type any path, so uninstall must never treat "it has a MiMoSwitch.exe in it"
// as licence to delete a folder that also holds someone's documents.
func OnlyOurs(dir string) (foreign []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.EqualFold(name, exeName) || strings.EqualFold(name, uninstallerName) ||
			strings.EqualFold(name, readmeName) {
			continue
		}
		foreign = append(foreign, name)
	}
	return foreign, nil
}

// AcceptableTarget rejects the paths that must never become an install folder: a drive root and
// the well-known user folders sitting directly in the profile. Installing there would mean
// uninstall deletes a folder the user cares about, and the picker can reach all of them.
func AcceptableTarget(dir string) error {
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) == clean {
		return fmt.Errorf("%s 是盘符根目录，请选一个专门放本程序的文件夹，例如 D:\\MiMoSwitch", clean)
	}
	volume := filepath.VolumeName(clean)
	if clean == volume+string(filepath.Separator) {
		return fmt.Errorf("%s 是盘符根目录，请选一个专门放本程序的文件夹，例如 D:\\MiMoSwitch", clean)
	}
	for _, reserved := range reservedDirs() {
		if strings.EqualFold(clean, filepath.Clean(reserved)) {
			return fmt.Errorf("%s 是系统常用的用户目录，不能当安装位置。请另选一个空文件夹，例如 D:\\MiMoSwitch", clean)
		}
	}
	return nil
}

func reservedDirs() []string {
	var out []string
	add := func(v string) {
		if v != "" {
			out = append(out, v)
		}
	}
	add(os.Getenv("USERPROFILE"))
	add(os.Getenv("APPDATA"))
	add(os.Getenv("LOCALAPPDATA"))
	add(os.Getenv("TEMP"))
	home := os.Getenv("USERPROFILE")
	if home != "" {
		for _, name := range []string{"Desktop", "Documents", "Downloads", "Pictures", "Music", "Videos"} {
			add(filepath.Join(home, name))
		}
	}
	return out
}

// ChooseDir shows the native folder picker once, at install time. "" means the user cancelled.
//
// It goes through Shell.Application instead of WinForms' FolderBrowserDialog because that one
// was observed sitting inside ShowDialog having never put a window on screen, while this one
// shows normally — verified side by side on the same machine.
func ChooseDir(start string) (string, error) {
	// 0x10 accept only file-system folders, 0x140 give the resizable dialog with an edit box.
	script := "$f = (New-Object -ComObject Shell.Application).BrowseForFolder(0, " +
		psQuote("选择 MiMo Switch 的安装位置（需当前用户可写，例如 D:\\MiMoSwitch）") + ", 0x150, " +
		psQuote(start) + "); " +
		"if ($f) { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false; " +
		"Write-Output $f.Self.Path }"
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
	// CREATE_NO_WINDOW rather than HideWindow: the latter marks the child STARTF_USESHOWWINDOW
	// with SW_HIDE, which is exactly how a dialog ends up running invisibly.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("打开目录选择框失败: %w", err)
	}
	// Redirected PowerShell output can carry a BOM; and a stray trailing line from a profile
	// must not end up inside the path we later delete recursively.
	picked := strings.TrimPrefix(strings.TrimSpace(string(out)), "\uFEFF")
	if i := strings.IndexByte(picked, '\n'); i >= 0 {
		picked = strings.TrimSpace(picked[:i])
	}
	return picked, nil
}

// Relocate copies the running distributable into its target folder and returns the new path,
// or "" when nothing needs doing. Only the GUI build is named MiMoSwitch.exe, so the console
// build used while developing never moves itself.
func Relocate(recorded string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if !IsDistributable(self) {
		return "", nil
	}
	dir, exe, err := Target(recorded)
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
	if err := writeCopy(self, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return exe, nil
}

// EnsureUninstaller keeps the folder's 卸载 MiMo Switch.exe in step with the main exe: created
// when absent, rewritten when the main exe is newer (an upgrade), otherwise left alone, since
// this runs on every launch.
func EnsureUninstaller(exe string) error {
	src, err := os.Stat(exe)
	if err != nil {
		return err
	}
	dst := filepath.Join(filepath.Dir(exe), uninstallerName)
	if info, err := os.Stat(dst); err == nil && info.Size() == src.Size() && !info.ModTime().Before(src.ModTime()) {
		return nil
	}
	tmp := dst + ".new"
	if err := writeCopy(exe, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// EnsureReadme drops the 使用说明 beside the exe on every launch when its content drifted,
// so the documentation lives in the install folder instead of the download the user deletes.
func EnsureReadme(exe string) error {
	dst := filepath.Join(filepath.Dir(exe), readmeName)
	if got, err := os.ReadFile(dst); err == nil && string(got) == readmeText {
		return nil
	}
	return os.WriteFile(dst, []byte(readmeText), 0o600)
}

// writeCopy copies src to dst with 0700, leaving dst untouched if the copy fails midway.
func writeCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// StartMenu puts the app and its uninstaller entry in the user's Programs folder, which is
// writable without elevation.
func StartMenu(recorded string) error {
	programs, err := programsDir()
	if err != nil {
		return err
	}
	dir, exe, err := Target(recorded)
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

// RestageSelf copies the running executable next to the temp folder and returns the new path.
// Windows refuses to delete a loaded image, and renaming it out only works on the same volume,
// so an install on D: could never delete itself: the folder kept whatever exe was running.
// Continuing the uninstall from a copy elsewhere leaves the chosen folder with nothing loaded
// in it, and the copy is swept on the next launch.
func RestageSelf() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	staged := filepath.Join(os.TempDir(), stagedPrefix+fmt.Sprint(time.Now().UnixNano()))
	if err := writeCopy(self, staged); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("复制卸载副本失败: %w", err)
	}
	return staged, nil
}

// IsStaged reports whether this executable is the temp copy an uninstall continues from.
func IsStaged(exe string) bool {
	return strings.HasPrefix(filepath.Base(exe), stagedPrefix)
}

// SweepAfterExit asks a detached helper to delete path once this process is gone. A running
// image cannot delete itself — Windows denies DELETE on a loaded executable, measured rather
// than assumed — so the copy an uninstall continued from has to be removed by something that
// outlives it. Failing to schedule that is not fatal: SweepStale still gets it on the next
// launch, which just leaves it in the temp folder until then.
func SweepAfterExit(path string) error {
	// Wait for our own pid to disappear rather than sleeping blind, then keep trying: the loader
	// can still hold the image for a moment after we exit, and one swallowed Remove-Item is
	// exactly how the copy ended up surviving the sweep.
	script := fmt.Sprintf(
		"$p=%d; $i=0; while ((Get-Process -Id $p -ErrorAction SilentlyContinue) -and $i -lt 200) { $i++; Start-Sleep -Milliseconds 300 }; "+
			"for ($j=0; $j -lt 40; $j++) { try { Remove-Item -LiteralPath %s -Force -ErrorAction Stop; break } catch { Start-Sleep -Milliseconds 300 } }",
		os.Getpid(), psQuote(path))
	// Measured on a real machine: a plain powershell child with CREATE_NO_WINDOW does outlive
	// this process, while DETACHED_PROCESS fails to start at all. Do not wrap this in
	// "cmd /c start" — that works too, but only hides which flag is doing the work.
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-WindowStyle", "Hidden", "-EncodedCommand", encodePSCommand(script))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("安排临时副本清理失败: %w", err)
	}
	return nil
}

// encodePSCommand renders a script the way -EncodedCommand expects: UTF-16LE, base64. Passing
// the script this way keeps paths with spaces, quotes and Chinese out of the quoting business,
// which is where an earlier version of this code broke.
func encodePSCommand(script string) string {
	units, err := windows.UTF16FromString(script)
	if err != nil {
		units = windows.StringToUTF16(script)
	}
	out := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[i*2:], u)
	}
	return base64.StdEncoding.EncodeToString(out)
}

// SweepStale deletes uninstall copies that a killed helper or an abnormal exit left behind.
// SweepAfterExit is the normal route; this is the backstop. Temp files can still be locked when
// we try, so failures are ignored and retried on the next launch.
func SweepStale() {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), stagedPrefix+"*"))
	if err != nil {
		return
	}
	for _, path := range matches {
		_ = os.Remove(path)
	}
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
