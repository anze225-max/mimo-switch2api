// Package tray runs the proxy as a silent resident app: no console window, logging to a
// file, with a status-area icon for the panel and shutdown.
package tray

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/getlantern/systray"

	"mimo-switch/internal/autostart"
	"mimo-switch/internal/server"
	"mimo-switch/internal/store"
)

//go:embed assets/tray.ico
var icon []byte

// DetachConsole frees the inherited console so a logon-launched process never shows a
// window. It is a no-op in a GUI-subsystem build, which has no console to begin with.
func DetachConsole() {
	if runtime.GOOS != "windows" {
		return
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	if proc := kernel32.NewProc("FreeConsole"); proc != nil {
		_, _, _ = proc.Call()
	}
}

// redirectLogsToFile keeps diagnostics available even though nothing is on screen.
func redirectLogsToFile() (string, error) {
	dir, err := store.Dir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "tray.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	os.Stdout = f
	os.Stderr = f
	return path, nil
}

// Run starts the proxy and blocks in the status-area loop.
func Run() error {
	DetachConsole()
	logPath, err := redirectLogsToFile()
	if err != nil {
		return err
	}

	cfg, err := store.Load()
	if err != nil {
		return err
	}
	srv, err := server.New(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	serverErr := make(chan error, 1)
	go func() { serverErr <- srv.ListenAndServe(ctx) }()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-serverErr:
		cancel()
		return fmt.Errorf("本地端点启动失败: %w", err)
	default:
	}

	fmt.Fprintf(os.Stderr, "tray started on %s (log: %s)\n", cfg.Listen, logPath)

	systray.Run(func() { ready(cfg, cancel, serverErr) }, func() { cancel() })
	return nil
}

func ready(cfg *store.Config, cancel context.CancelFunc, serverErr chan error) {
	systray.SetIcon(icon)
	systray.SetTitle("MiMo Switch")
	systray.SetTooltip("MiMo 免费额度本地端点 · http://" + cfg.Listen + "/v1")

	heading := systray.AddMenuItem("MiMo Switch", "本地端点 http://"+cfg.Listen+"/v1")
	heading.Disable()
	openPanel := systray.AddMenuItem("打开主界面", "查看用量与各家工具的接入配置")
	toggleAuto := systray.AddMenuItem("开机静默自启", "登录时自动启动，不显示任何窗口")
	systray.AddSeparator()
	quit := systray.AddMenuItem("退出应用", "停止本地端点并退出")

	if on, _, err := autostart.Enabled(); err == nil && on {
		toggleAuto.Check()
	}

	go func() {
		for {
			select {
			case <-openPanel.ClickedCh:
				openInBrowser("http://" + cfg.Listen + "/")
			case <-toggleAuto.ClickedCh:
				if toggleAuto.Checked() {
					toggleAuto.Uncheck()
					_ = autostart.Disable()
				} else {
					if err := autostart.Enable(); err == nil {
						toggleAuto.Check()
					}
				}
			case <-quit.ClickedCh:
				cancel()
				systray.Quit()
				return
			case err := <-serverErr:
				if err != nil {
					fmt.Fprintln(os.Stderr, "server stopped:", err)
				}
				systray.Quit()
				return
			}
		}
	}()
}

func openInBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
