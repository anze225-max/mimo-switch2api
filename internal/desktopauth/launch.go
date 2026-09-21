package desktopauth

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// LaunchMiMo starts the MiMo desktop app with its Node inspector port so a session can be
// harvested without the user editing shortcuts. It is only needed once per login, because
// the serviceToken lives in the app's live cookie jar.
func LaunchMiMo(exePath string, port int) error {
	exe, err := resolveMiMoExe(exePath)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, fmt.Sprintf("--inspect=%d", port))
	cmd.Dir = filepath.Dir(exe)
	// Detach so the app outlives this process and keeps its own window.
	cmd.SysProcAttr = detachAttr()
	return cmd.Start()
}

// resolveMiMoExe finds the installed app: explicit path, then the common install roots.
func resolveMiMoExe(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("找不到 %s", explicit)
		}
		return explicit, nil
	}
	candidates := []string{
		`D:\XiaomiMimo\Xiaomi MiMo\Xiaomi MiMo.exe`,
		`C:\Program Files\Xiaomi MiMo\Xiaomi MiMo.exe`,
		`C:\Program Files (x86)\Xiaomi MiMo\Xiaomi MiMo.exe`,
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates, filepath.Join(local, `Programs\Xiaomi MiMo\Xiaomi MiMo.exe`))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("没找到 MiMo 主程序，请用 --exe 指定 Xiaomi MiMo.exe 的路径")
}

// WaitReady blocks until the inspector port answers, or gives up.
func WaitReady(port int, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		res, err := client.Get(url)
		if err == nil {
			res.Body.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("等不到 MiMo 在端口 %d 上就绪", port)
}

// Running reports whether a MiMo process is already up, so we do not launch a second one.
func Running() bool {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq Xiaomi MiMo.exe", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Xiaomi MiMo.exe")
}
