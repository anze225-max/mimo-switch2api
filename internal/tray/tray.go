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
	"mimo-switch/internal/login"
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
	// The call fails when no console is attached, which is the normal case for a
	// GUI-subsystem build, so the status is not interesting.
	_, _, _ = kernel32.NewProc("FreeConsole").Call()
}

// redirectLogsToFile keeps diagnostics available even though nothing is on screen.
func redirectLogsToFile() (string, error) {
	dir, err := store.Dir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "tray.log")
	// A resident proxy runs for weeks, so roll the log over instead of appending forever.
	const maxLogBytes = 2 << 20
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
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

	systray.Run(func() { ready(srv, cfg, cancel, serverErr) }, func() { cancel() })
	return nil
}

func ready(srv *server.Server, cfg *store.Config, cancel context.CancelFunc, serverErr chan error) {
	systray.SetIcon(icon)
	systray.SetTitle("MiMo Switch")
	systray.SetTooltip("MiMo 免费额度本地端点 · http://" + cfg.Listen + "/v1")

	heading := systray.AddMenuItem("MiMo Switch", "本地端点 http://"+cfg.Listen+"/v1")
	heading.Disable()
	openPanel := systray.AddMenuItem("打开主界面", "查看用量与各家工具的接入配置")
	loginItem := systray.AddMenuItem("登录／重新授权小米账号", "在本工具打开的窗口里登录，无需打开 MiMo")
	refresh := systray.AddMenuItem("立即续期会话", "用 passToken 静默换取新的 serviceToken，无需打开 MiMo")
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
			case <-loginItem.ClickedCh:
				switch err := srv.StartLogin(); {
				case err != nil:
					loginItem.SetTitle("登录失败：" + err.Error())
				default:
					loginItem.SetTitle("等待你在窗口里完成登录…")
					// Give the window its own timeout plus a minute before the menu claims
					// to be idle again; adoption itself happens in the background.
					time.AfterFunc(login.DefaultTimeout+time.Minute, func() {
						loginItem.SetTitle("登录／重新授权小米账号")
					})
				}
			case <-refresh.ClickedCh:
				if err := srv.RefreshNow(); err != nil {
					refresh.SetTitle("续期失败：" + err.Error())
				} else {
					refresh.SetTitle("已续期")
				}
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

// OpenURL shows an address in the user's default browser.
func OpenURL(url string) { openInBrowser(url) }

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
