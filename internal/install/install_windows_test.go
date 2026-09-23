//go:build windows

package install

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestTargetUsesRecordedDir(t *testing.T) {
	recorded := filepath.Join(`D:\`, "Apps", "MiMoSwitch")
	dir, exe, err := Target(recorded)
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if dir != recorded {
		t.Errorf("dir = %q, want %q", dir, recorded)
	}
	if filepath.Base(exe) != exeName || filepath.Dir(exe) != recorded {
		t.Errorf("exe = %q, want %s inside %s", exe, exeName, recorded)
	}
}

func TestTargetFallsBackToDefault(t *testing.T) {
	dir, _, err := Target("")
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if filepath.Base(dir) != dirName {
		t.Errorf("default dir = %q, want it to end in %q", dir, dirName)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("default dir must be absolute, got %q", dir)
	}
}

func TestTargetRejectsRelativeRecordedDir(t *testing.T) {
	// A relative record would make the app anchor into whatever the current directory
	// happens to be, which is how you end up deleting the wrong folder on uninstall.
	for _, bad := range []string{"MiMoSwitch", `D:Apps\MiMoSwitch`} {
		if _, _, err := Target(bad); err == nil {
			t.Errorf("install dir %q accepted, want error", bad)
		}
	}
}

func TestNameClassification(t *testing.T) {
	cases := []struct {
		path          string
		distributable bool
		uninstaller   bool
	}{
		{filepath.Join("D:", "x", "MiMoSwitch.exe"), true, false},
		{filepath.Join("D:", "x", "mimoswitch.EXE"), true, false},
		{filepath.Join("D:", "x", uninstallerName), false, true},
		{filepath.Join("D:", "x", "mimo-switch.exe"), false, false},
		{filepath.Join("D:", "x", "卸载 MiMo Switch.exe"), false, true},
	}
	for _, tc := range cases {
		if got := IsDistributable(tc.path); got != tc.distributable {
			t.Errorf("IsDistributable(%q) = %v, want %v", tc.path, got, tc.distributable)
		}
		if got := IsUninstaller(tc.path); got != tc.uninstaller {
			t.Errorf("IsUninstaller(%q) = %v, want %v", tc.path, got, tc.uninstaller)
		}
	}
}

func TestEnsureUninstallerCreatesAndSkips(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, exeName)
	writeFakeExe(t, exe, 64)

	if err := EnsureUninstaller(exe); err != nil {
		t.Fatalf("EnsureUninstaller: %v", err)
	}
	copyPath := filepath.Join(dir, uninstallerName)
	if !sameContents(t, exe, copyPath) {
		t.Fatal("uninstaller copy does not match the main exe")
	}
	if !LooksInstalled(dir) {
		t.Error("LooksInstalled false right after installing")
	}

	// Untouched source must not be re-copied: this runs on every launch.
	infoBefore, err := os.Stat(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureUninstaller(exe); err != nil {
		t.Fatalf("EnsureUninstaller (second pass): %v", err)
	}
	infoAfter, err := os.Stat(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Errorf("idle launch rewrote the copy: %v -> %v", infoBefore.ModTime(), infoAfter.ModTime())
	}
}

func TestEnsureUninstallerRefreshesAfterUpgrade(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, exeName)
	writeFakeExe(t, exe, 64)
	if err := EnsureUninstaller(exe); err != nil {
		t.Fatal(err)
	}

	writeFakeExe(t, exe, 128)
	if err := EnsureUninstaller(exe); err != nil {
		t.Fatal(err)
	}
	if !sameContents(t, exe, filepath.Join(dir, uninstallerName)) {
		t.Error("copy still holds the old bytes after the main exe changed")
	}
}

func TestEnsureUninstallerReplacesMissingCopy(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, exeName)
	writeFakeExe(t, exe, 64)
	if err := EnsureUninstaller(exe); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, uninstallerName)); err != nil {
		t.Fatal(err)
	}
	if err := EnsureUninstaller(exe); err != nil {
		t.Fatalf("EnsureUninstaller: %v", err)
	}
	if !LooksInstalled(dir) {
		t.Error("missing copy was not restored")
	}
}

func TestEnsureReadmeWritesRefreshesAndLeavesAlone(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, exeName)
	writeFakeExe(t, exe, 64)
	dst := filepath.Join(dir, readmeName)

	if err := EnsureReadme(exe); err != nil {
		t.Fatalf("EnsureReadme: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != readmeText {
		t.Fatal("README.txt does not carry the embedded 使用说明")
	}

	// Untouched content must not be rewritten: this runs on every launch.
	infoBefore, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureReadme(exe); err != nil {
		t.Fatalf("EnsureReadme (second pass): %v", err)
	}
	infoAfter, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Errorf("idle launch rewrote the readme: %v -> %v", infoBefore.ModTime(), infoAfter.ModTime())
	}

	// A hand-edited file is put back: the doc must not go stale or wrong across upgrades.
	if err := os.WriteFile(dst, []byte("旧版说明"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureReadme(exe); err != nil {
		t.Fatalf("EnsureReadme (after edit): %v", err)
	}
	got, err = os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != readmeText {
		t.Error("tampered README.txt was not refreshed")
	}
}

func TestLooksInstalledRejectsForeignDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if LooksInstalled(dir) {
		t.Fatal("a folder without our executables looks installed")
	}
	if LooksInstalled(filepath.Join(dir, "nope")) {
		t.Fatal("a missing folder looks installed")
	}
}

func TestEncodePSCommandSurvivesAChinesePath(t *testing.T) {
	script := `Remove-Item -LiteralPath 'D:\新建文件夹\卸载 MiMo Switch.exe' -Force`
	raw, err := base64.StdEncoding.DecodeString(encodePSCommand(script))
	if err != nil {
		t.Fatalf("not base64: %v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("UTF-16 payload has odd length %d", len(raw))
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	if got := strings.TrimRight(string(utf16.Decode(units)), "\x00"); got != script {
		t.Errorf("round trip gave %q, want %q", got, script)
	}
}

func TestIsStagedRecognisesOnlyStagedCopies(t *testing.T) {
	cases := map[string]bool{
		filepath.Join(os.TempDir(), stagedPrefix+"1790136807684983800"): true,
		filepath.Join("D:", "x", exeName):                               false,
		filepath.Join("D:", "x", uninstallerName):                       false,
		filepath.Join("D:", "x", "mimo-switch.exe"):                     false,
	}
	for path, want := range cases {
		if got := IsStaged(path); got != want {
			t.Errorf("IsStaged(%q) = %v, want %v", path, got, want)
		}
	}
}

// The picker lets the user reach a drive root or their own Documents folder. Installing there
// would hand uninstall a folder full of someone's files.
func TestAcceptableTargetRejectsFoldersThatMustSurvive(t *testing.T) {
	t.Setenv("USERPROFILE", `C:\Users\me`)
	t.Setenv("APPDATA", `C:\Users\me\AppData\Roaming`)
	t.Setenv("LOCALAPPDATA", `C:\Users\me\AppData\Local`)
	t.Setenv("TEMP", `C:\Users\me\AppData\Local\Temp`)

	cases := []struct {
		dir     string
		wantErr bool
	}{
		{`D:\MiMoSwitch`, false},
		{`D:\新建文件夹`, false},
		{filepath.Join(os.Getenv("LOCALAPPDATA"), "MiMoSwitch"), false},
		{os.Getenv("LOCALAPPDATA"), true},
		{os.Getenv("APPDATA"), true},
		{os.Getenv("TEMP"), true},
		{`C:\Users\me`, true},
		{`C:\Users\me\Documents`, true},
		{`C:\Users\me\Desktop`, true},
		{`D:\`, true},
		{`C:\`, true},
	}
	for _, tc := range cases {
		err := AcceptableTarget(tc.dir)
		if (err != nil) != tc.wantErr {
			t.Errorf("AcceptableTarget(%q) gave err=%v, wantErr=%v", tc.dir, err, tc.wantErr)
		}
	}
}

// A download folder holding one copy of the exe is not an install; only an installed folder ever
// sees the uninstaller copy.
func TestHasUninstallerSeparatesInstallFromDownload(t *testing.T) {
	download := t.TempDir()
	if err := os.WriteFile(filepath.Join(download, exeName), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if HasUninstaller(download) {
		t.Fatal("a lone downloaded exe looks like an install")
	}
	if err := os.WriteFile(filepath.Join(download, uninstallerName), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !HasUninstaller(download) {
		t.Fatal("an install folder with the uninstaller copy is not recognised")
	}
}

func TestOnlyOursFlagsForeignFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{exeName, uninstallerName, readmeName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if foreign, err := OnlyOurs(dir); err != nil || len(foreign) != 0 {
		t.Fatalf("a folder with only our files reported foreign=%v err=%v", foreign, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "我的论文.docx"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign, err := OnlyOurs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 1 || foreign[0] != "我的论文.docx" {
		t.Errorf("foreign = %v, want the one document", foreign)
	}
}

func writeFakeExe(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o700); err != nil {
		t.Fatal(err)
	}
	// The skip rule compares mtimes, and a fresh copy is stamped now, so the source has to
	// look older than it. Sizes differ per case, which keeps the two writes distinguishable.
	age := time.Duration(size) * time.Millisecond
	if err := os.Chtimes(path, time.Now().Add(-age), time.Now().Add(-age)); err != nil {
		t.Fatal(err)
	}
}

func sameContents(t *testing.T, a, b string) bool {
	t.Helper()
	ab, err := os.ReadFile(a)
	if err != nil {
		t.Fatalf("read %s: %v", a, err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatalf("read %s: %v", b, err)
	}
	return string(ab) == string(bb)
}
