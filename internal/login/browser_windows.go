//go:build windows

package login

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

// browserCandidates are the stock install paths. Edge ships with every Windows 11 box, so it
// goes first; Chrome is the fallback.
var browserCandidates = []string{
	`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
}

func browserPath() (string, error) {
	for _, p := range browserCandidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("找不到 Edge 或 Chrome，无法打开登录窗口")
}

// openLoginWindow launches a browser against a private profile, so the login the user performs
// here is invisible to their everyday browser and the reverse tool never touches the profile
// they actually use.
func openLoginWindow(exe, profile string, port int, url string) (*exec.Cmd, error) {
	cmd := exec.Command(exe,
		"--user-data-dir="+profile,
		"--remote-debugging-port="+strconv.Itoa(port),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
		"--hide-crash-restore-bubble",
		"--app="+url,
	)
	return cmd, cmd.Start()
}

// killTree is necessary because a browser forks renderer and GPU processes that outlive its
// parent; killing only the parent would leave them holding the profile we are about to delete.
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
