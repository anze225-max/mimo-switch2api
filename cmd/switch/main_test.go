package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mimo-switch/internal/install"
	"mimo-switch/internal/store"
)

func TestExistingInstanceRecognisesOurOwnHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","plan":"desktop-free","upstream":"https://example.invalid/route"}`))
	}))
	defer srv.Close()

	if got := existingInstance(srv.Listener.Addr().String()); got == "" {
		t.Errorf("existingInstance = %q, want the address of the running proxy", got)
	}
}

// Someone else's server on the port must not silence us: the bind error is the diagnosis the
// user needs, whereas a quiet exit looks like the app refusing to start.
func TestExistingInstanceIgnoresForeignServers(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer foreign.Close()
	if got := existingInstance(foreign.Listener.Addr().String()); got != "" {
		t.Errorf("existingInstance = %q for a server without an upstream, want \"\"", got)
	}
}

// Backing out of the folder picker must leave nothing behind: no copied exe, no recorded
// location, no half-installed folder to trip over on the next double-click.
func TestInstallNowCancelLeavesNothing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local"))
	t.Setenv("APPDATA", filepath.Join(root, "roaming"))

	download := filepath.Join(root, "Downloads")
	if err := os.MkdirAll(download, 0o700); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(download, "MiMoSwitch.exe")
	if err := os.WriteFile(self, []byte("fake PE"), 0o700); err != nil {
		t.Fatal(err)
	}

	asked := 0
	restorePickDir(t, func(string) (string, error) { asked++; return "", nil })

	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := installNow(cfg, self); !errors.Is(err, errInstallCancelled) {
		t.Fatalf("installNow = %v, want %v", err, errInstallCancelled)
	}
	if asked != 1 {
		t.Errorf("picker opened %d times, want 1", asked)
	}
	if cfg.InstallDir != "" {
		t.Errorf("InstallDir = %q, want it left empty", cfg.InstallDir)
	}
	if _, err := os.Stat(filepath.Join(root, "local", "MiMoSwitch")); !os.IsNotExist(err) {
		t.Errorf("a cancelled install still created the target folder")
	}
}

// A copy that already lives in its folder must not be asked where to go, and must adopt that
// folder as the record — otherwise an install made before this option existed would keep
// moving itself, and prompting, on every launch.
func TestInstallNowAdoptsFolderItAlreadyLivesIn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local"))
	t.Setenv("APPDATA", filepath.Join(root, "roaming"))

	home := filepath.Join(root, "local", "MiMoSwitch")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(home, "MiMoSwitch.exe")
	if err := os.WriteFile(self, []byte("fake PE"), 0o700); err != nil {
		t.Fatal(err)
	}
	asked := 0
	restorePickDir(t, func(string) (string, error) { asked++; return "", nil })

	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := installNow(cfg, self); err != nil {
		t.Fatalf("installNow: %v", err)
	}
	if asked != 0 {
		t.Errorf("picker opened %d times for an already-installed copy, want 0", asked)
	}
	if cfg.InstallDir != home {
		t.Errorf("InstallDir = %q, want %q", cfg.InstallDir, home)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if install.IsUninstaller(filepath.Join(home, e.Name())) {
			found = true
		}
	}
	if !found {
		t.Error("no uninstaller copy next to the main exe")
	}

	reloaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.InstallDir != home {
		t.Errorf("record did not survive the save: %q", reloaded.InstallDir)
	}
}

func restorePickDir(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	previous := pickDir
	pickDir = fn
	t.Cleanup(func() { pickDir = previous })
}

// The install folder can sit on another volume than the temp copy the uninstall continued
// from, and an Explorer window left inside it holds the directory node. Either way nothing of
// ours may survive, and a folder that is merely emptied must not be reported as a failure —
// which is what holdDir reproduces: Windows refuses to delete a directory someone is watching,
// even once nothing is left in it.
func TestRemoveDirLeavesNothingOfOurs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "新建文件夹")
	for _, rel := range []string{"MiMoSwitch.exe", "卸载 MiMo Switch.exe", filepath.Join("sub", "file.txt")} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	release := holdDir(t, dir)
	defer release()

	if err := removeDir(dir); err != nil {
		t.Fatalf("removeDir with the folder held open: %v", err)
	}
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		t.Log("目录节点连占用一起被删了，本例只覆盖到普通删除路径")
	case err != nil:
		t.Fatalf("read %s: %v", dir, err)
	case len(entries) != 0:
		t.Errorf("files survived the uninstall: %v", entries)
	}
}

// The real reason an uninstall used to leave the folder behind: a process cannot delete its own
// working directory, and double-clicking 卸载 MiMo Switch.exe from inside the install folder
// makes that folder the working directory. Every file goes, the node stays.
func TestRemoveDirAfterSteppingOutOfTarget(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "新建文件夹")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MiMoSwitch.exe"), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	stepOutOfTarget()
	if err := removeDir(dir); err != nil {
		t.Fatalf("removeDir after stepping out of the target: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("folder %s still exists", dir)
	}
}

// Moving the install folder by hand must not produce a second copy elsewhere: the app follows
// the folder it is actually running from.
func TestInstallNowFollowsAMovedFolder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local"))
	t.Setenv("APPDATA", filepath.Join(root, "roaming"))

	moved := filepath.Join(root, "MiMoSwitch")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(moved, "MiMoSwitch.exe")
	if err := os.WriteFile(self, []byte("fake PE"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moved, "卸载 MiMo Switch.exe"), []byte("fake PE"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &store.Config{InstallDir: filepath.Join(root, "old")}
	if err := installNow(cfg, self); err != nil {
		t.Fatalf("installNow: %v", err)
	}
	if cfg.InstallDir != moved {
		t.Errorf("InstallDir = %q, want the folder it is running from (%q)", cfg.InstallDir, moved)
	}
	reloaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.InstallDir != moved {
		t.Errorf("record did not survive the save: %q", reloaded.InstallDir)
	}
}

func TestDirEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !dirEmpty(dir) {
		t.Error("an empty folder does not read as empty")
	}
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if dirEmpty(dir) {
		t.Error("a folder with a file reads as empty")
	}
	if dirEmpty(filepath.Join(dir, "missing")) {
		t.Error("a missing folder reads as empty")
	}
}
