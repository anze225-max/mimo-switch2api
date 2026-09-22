package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"mimo-switch/internal/auth"
	"mimo-switch/internal/autostart"
	"mimo-switch/internal/desktopauth"
	"mimo-switch/internal/install"
	"mimo-switch/internal/login"
	"mimo-switch/internal/notify"
	"mimo-switch/internal/server"
	"mimo-switch/internal/store"
	"mimo-switch/internal/tray"
	"mimo-switch/internal/upstream"
	"mimo-switch/internal/usage"
)

func main() {
	flag.Usage = printUsage
	flag.Parse()
	if err := run(flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		if isInstalledBuild() {
			// A double-clicked GUI build has no console, so stderr alone would be invisible.
			notify.Error("MiMo Switch 启动失败", err)
		}
		os.Exit(1)
	}
}

// isInstalledBuild reports whether this is the distributable GUI binary, the only one that
// should ever speak through a dialog box. A console build may run without a Windows console
// (Git Bash hands out a pseudo-terminal), so testing for a console misclassifies it.
func isInstalledBuild() bool {
	exe, err := os.Executable()
	return err == nil && install.IsDistributable(exe)
}

func printUsage() {
	fmt.Fprint(os.Stderr, `mimo-switch — 把 MiMo 免费额度暴露成本地 OpenAI/Anthropic 端点

用法:
  mimo-switch login                在本工具打开的小米登录窗口里登录，取回桌面端免费额度（推荐）
  mimo-switch refresh              用已存的 passToken 静默续期（无需 MiMo 运行）
  mimo-switch serve                启动本地反代
  mimo-switch tray                 后台运行 + 托盘图标（静默，无控制台）
  mimo-switch autostart on|off     开机静默自启
  mimo-switch uninstall            一键卸载：删自启项、快捷方式、程序目录与配置（--keep-config 保留凭证）
  mimo-switch status               查看已保存的凭证（掩码显示）
  mimo-switch models               用当前凭证拉取上游模型列表，验证可用
  mimo-switch probe [model]        发一条最小补全，验证端到端

  其它:
  mimo-switch authorize            打开官方授权页签发 API key（需小米付费套餐）
  mimo-switch authorize --code X   授权页显示 code 时的手动兜底
  mimo-switch harvest [--file F]   兜底：从已在运行且开了调试端口的 MiMo 取会话
  mimo-switch harvest --launch     兜底：带调试端口启动 MiMo，从它的会话里接管（需要装 MiMo）
`)
}

func run(args []string) error {
	if len(args) == 0 {
		if isInstalledBuild() {
			return cmdTray() // double-clicked: go straight to the silent tray app
		}
		printUsage()
		return fmt.Errorf("缺少子命令")
	}
	switch args[0] {
	case "authorize":
		return cmdAuthorize(args[1:])
	case "login":
		return cmdLogin()
	case "harvest":
		return cmdHarvest(args[1:])
	case "refresh":
		return cmdRefresh()
	case "status":
		return cmdStatus()
	case "models":
		return cmdModels()
	case "probe":
		return cmdProbe(args[1:])
	case "serve":
		return cmdServe(args[1:])
	case "tray":
		return cmdTray()
	case "autostart":
		return cmdAutostart(args[1:])
	case "uninstall":
		return cmdUninstall(args[1:])
	default:
		printUsage()
		return fmt.Errorf("未知子命令 %q", args[0])
	}
}

func cmdAuthorize(args []string) error {
	fs := flag.NewFlagSet("authorize", flag.ExitOnError)
	code := fs.String("code", "", "授权页给出的 code（无 localhost 回调时使用）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := store.Dir()
	if err != nil {
		return err
	}
	sess, err := auth.StartSession(dir)
	if err != nil {
		return err
	}
	defer sess.Close()

	if *code != "" {
		res, err := sess.Handcode(*code)
		if err != nil {
			return err
		}
		return finish(res, dir)
	}

	fmt.Println("正在打开授权页面…若浏览器没有自动打开，请手动访问：")
	fmt.Println()
	fmt.Println("  " + sess.AuthorizeURL())
	fmt.Println()
	fmt.Println("等待你在浏览器里用小米账号确认（5 分钟超时）。")
	fmt.Println("若页面改为显示一个 code，请用: mimo-switch authorize --code <code>")
	if err := sess.OpenBrowser(); err != nil {
		fmt.Println("（无法自动打开浏览器，请手动访问上面的链接）")
	}

	ctx, cancel := context.WithTimeout(context.Background(), auth.DefaultTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			fmt.Println("\n等待超时，未收到授权结果。")
		case <-done:
		}
	}()
	res, err := sess.Wait(ctx)
	close(done)
	if err != nil {
		return err
	}
	return finish(res, dir)
}

func finish(res *auth.Result, dir string) error {
	if res.URL == "" {
		res.URL = "https://api.xiaomimimo.com/v1"
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	cred := &store.Credential{
		SK:       res.SK,
		BaseURL:  res.URL,
		UID:      res.UID,
		KeyName:  keyNameOf(dir),
		IssuedAt: time.Now().UTC(),
	}
	cfg.SetCredential(cred)
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Println("授权成功。")
	fmt.Printf("  key      : %s\n", cred.Masked())
	fmt.Printf("  base_url : %s\n", cred.BaseURL)
	fmt.Printf("  uid      : %s\n", cred.UID)
	fmt.Printf("  plan     : %s\n", cred.Plan())
	fmt.Printf("  key_name : %s\n", cred.KeyName)
	return nil
}

func keyNameOf(dir string) string {
	name, err := auth.KeyName(dir)
	if err != nil {
		return ""
	}
	return name
}

func requireCredential() (*store.Config, *store.Credential, error) {
	cfg, err := store.Load()
	if err != nil {
		return nil, nil, err
	}
	cred := cfg.Credential()
	if cred == nil {
		return nil, nil, fmt.Errorf("还没有凭证，先运行: mimo-switch login")
	}
	return cfg, cred, nil
}

func cmdStatus() error {
	cfg, cred, err := requireCredential()
	if err != nil {
		return err
	}
	fmt.Printf("本地端点   : http://%s/v1\n", cfg.Listen)
	fmt.Printf("需要本地token: %v\n", cfg.RequireToken)
	fmt.Printf("凭证       : %s -> %s (%s)\n", cred.Masked(), cred.BaseURL, cred.Plan())
	fmt.Printf("签发时间   : %s\n", cred.IssuedAt.Local().Format(time.RFC3339))
	return nil
}

func cmdModels() error {
	_, cred, err := requireCredential()
	if err != nil {
		return err
	}
	ids, err := upstream.New(cred.SK, cred.BaseURL).Models(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("上游返回 %d 个模型:\n", len(ids))
	for _, id := range ids {
		fmt.Println("  -", id)
	}
	return nil
}

func cmdProbe(args []string) error {
	_, cred, err := requireCredential()
	if err != nil {
		return err
	}
	model := "mimo-v2.5-pro"
	if len(args) > 0 {
		model = args[0]
	}
	out, err := upstream.New(cred.SK, cred.BaseURL).Chat(context.Background(), map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with exactly: PONG"}},
		"max_tokens": 16,
	})
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("%s 响应:\n%s\n", model, raw)
	return nil
}

func cmdHarvest(args []string) error {
	fs := flag.NewFlagSet("harvest", flag.ExitOnError)
	file := fs.String("file", "", "直接读取 cookie 导出，跳过实时抓取")
	port := fs.Int("port", 0, "MiMo 的调试端口（默认依次尝试 9229、9222）")
	launch := fs.Bool("launch", false, "若 MiMo 未运行，自动以调试端口启动它")
	exe := fs.String("exe", "", "MiMo 主程序路径（配合 --launch 使用）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var session *desktopauth.Session
	switch {
	case *file != "":
		s, err := desktopauth.LoadExport(*file)
		if err != nil {
			return err
		}
		session = s
		fmt.Println("已读取导出文件。")
	case *launch:
		debugPort := *port
		if debugPort == 0 {
			debugPort = 9229
		}
		if !desktopauth.Running() {
			if err := desktopauth.LaunchMiMo(*exe, debugPort); err != nil {
				return err
			}
			fmt.Println("已启动 MiMo，请在其窗口里用小米账号登录。")
		} else {
			fmt.Println("MiMo 已在运行；若它不是以调试端口启动的，请先完全退出再试 --launch。")
		}
		if err := desktopauth.WaitReady(debugPort, 90*time.Second); err != nil {
			return err
		}
		fmt.Println("等待你在 MiMo 窗口中完成登录…（最多 5 分钟）")
		deadline := time.Now().Add(5 * time.Minute)
		for {
			s, _, err := desktopauth.HarvestLive(debugPort)
			if err == nil {
				session = s
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("登录超时: %w", err)
			}
			time.Sleep(2 * time.Second)
		}
		fmt.Println("登录已确认，会话已取回。")
	default:
		ports := []int{*port}
		if *port == 0 {
			ports = []int{9229, 9222}
		}
		var lastErr error
		for _, candidate := range ports {
			if candidate == 0 {
				continue
			}
			s, _, err := desktopauth.HarvestLive(candidate)
			if err == nil {
				session = s
				fmt.Printf("已从运行中的 MiMo (端口 %d) 取回会话。\n", candidate)
				break
			}
			lastErr = err
			fmt.Printf("端口 %d 不可用: %v\n", candidate, err)
		}
		if session == nil {
			if lastErr != nil {
				return fmt.Errorf("%w\n（也可先导出 cookie 再用 --file 指定；或让 MiMo 以 --inspect=9229 启动（要的是 Node 调试口，不是 Chrome 的））", lastErr)
			}
			return fmt.Errorf("未能取回会话")
		}
	}

	return adopt(session, "已接管 MiMo 桌面端免费额度会话")
}

// cmdTray is what a double-click gives you. If an instance already serves the port, open
// its panel instead of failing to bind; otherwise sign in when needed and run the tray.
func cmdTray() error {
	if err := relocate(); err != nil {
		return err
	}
	if err := ensureRegistered(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	if running := existingInstance(cfg.Listen); running != "" {
		fmt.Printf("已有一个 MiMo Switch 在运行（%s），为你打开主界面。\n", running)
		tray.OpenURL("http://" + running + "/")
		return nil
	}
	if err := ensureCredential(cfg); err != nil {
		return err
	}
	return tray.Run()
}

// relocate moves the distributable into %LOCALAPPDATA%\MiMoSwitch and hands over to that
// copy, so the logon entry and shortcuts point at a path that survives the download folder
// being cleaned out. Failing to move is never fatal: it just runs where it was.
func relocate() error {
	exe, err := install.Relocate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "自我安置失败，就地运行: %v\n", err)
		return nil
	}
	if exe == "" {
		return nil
	}
	fmt.Println("已安置到", exe, "，改由该位置继续运行。")
	child := exec.Command(exe, os.Args[1:]...)
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "从新位置启动失败，就地继续运行: %v\n", err)
		return nil
	}
	os.Exit(0)
	return nil
}

// ensureRegistered keeps the logon entry and the shortcuts pointed at wherever this copy
// actually lives. It only writes when something differs, so a normal launch costs a
// registry read.
func ensureRegistered() error {
	exe, err := os.Executable()
	if err != nil || !install.IsDistributable(exe) {
		return err
	}
	if on, command, err := autostart.Enabled(); err == nil && on && strings.Contains(command, exe) {
		return nil
	}
	return registerStartup()
}

// registerStartup points the logon entry at the installed copy and adds the Start Menu
// entries, including the one-click uninstaller.
func registerStartup() error {
	if err := autostart.Enable(); err != nil {
		return fmt.Errorf("注册开机自启失败: %w", err)
	}
	if err := install.StartMenu(); err != nil {
		return fmt.Errorf("创建开始菜单快捷方式失败: %w", err)
	}
	return nil
}

// cmdUninstall removes everything the app owns: logon entry, shortcuts, program folder and
// (unless asked to keep it) the config that holds the encrypted credential.
func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	keepConfig := fs.Bool("keep-config", false, "保留配置与凭证")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, _, err := install.Target()
	if err != nil {
		return err
	}
	steps := []string{"开机自启项", "开始菜单快捷方式", "程序目录 " + dir}
	if !*keepConfig {
		if cfgDir, err := store.Dir(); err == nil {
			steps = append(steps, "配置与凭证 "+cfgDir)
		}
	}
	if isInstalledBuild() && !notify.Confirm("卸载 MiMo Switch",
		"将删除：\n  · "+strings.Join(steps, "\n  · ")+
			"\n\n正在运行的本地端点会先退出。确定继续？") {
		fmt.Println("已取消卸载。")
		return nil
	}

	stopRunningInstance()
	if err := autostart.Disable(); err != nil {
		fmt.Fprintf(os.Stderr, "删除开机自启项失败: %v\n", err)
	}
	if err := install.RemoveStartMenu(); err != nil {
		fmt.Fprintf(os.Stderr, "删除快捷方式失败: %v\n", err)
	}
	if !*keepConfig {
		if cfgDir, err := store.Dir(); err == nil {
			if err := os.RemoveAll(cfgDir); err != nil {
				fmt.Fprintf(os.Stderr, "删除配置失败: %v\n", err)
			}
		}
	}
	if install.RunningFromTarget(dir) {
		if err := install.SelfDelete(dir); err != nil {
			return fmt.Errorf("安排删除程序目录失败: %w", err)
		}
		fmt.Println("程序目录将在本进程退出后删除。")
	} else if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "删除程序目录失败: %v\n", err)
	}
	fmt.Println("卸载完成。")
	if isInstalledBuild() {
		notify.Info("MiMo Switch 已卸载", "开机自启、快捷方式和配置均已移除。")
	}
	return nil
}

// stopRunningInstance asks a live tray to exit before files disappear under it. Silence is
// fine: nothing running is the common case when uninstalling.
func stopRunningInstance() {
	cfg, err := store.Load()
	if err != nil || cfg.LocalToken == "" {
		return
	}
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodPost, "http://"+cfg.Listen+"/api/shutdown", nil)
	if err != nil {
		return
	}
	req.Header.Set("authorization", "Bearer "+cfg.LocalToken)
	res, err := client.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	time.Sleep(400 * time.Millisecond)
}

// existingInstance returns the address of a MiMo Switch that already answers /health there,
// or "" when the port is free. Only our own payload counts, so an unrelated program holding
// the port still produces the bind error rather than a silent exit.
func existingInstance(listen string) string {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	res, err := client.Get("http://" + listen + "/health")
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	var health struct {
		Status   string `json:"status"`
		Upstream string `json:"upstream"`
	}
	if err := json.NewDecoder(res.Body).Decode(&health); err != nil {
		return ""
	}
	if health.Status == "ok" && health.Upstream != "" {
		return listen
	}
	return ""
}

// ensureCredential obtains a session on first run. MiMo never has to be installed or open:
// the window we open is Xiaomi's own passport page, and we read only that window's cookie.
func ensureCredential(cfg *store.Config) error {
	if cfg.Credential() != nil {
		return nil
	}
	session, err := signIn()
	if err != nil {
		// Passport still refuses the in-tool sign-in for some accounts, so name the path
		// that works instead of leaving a dead end behind a double-click.
		return fmt.Errorf("%w\n首次取凭证请改用: MiMoSwitch.exe harvest --launch（需要本机装有 MiMo 客户端）", err)
	}
	return adopt(session, "首次登录完成，凭证已保存")
}

// signIn opens the login window and blocks until the user is through or the window dies.
func signIn() (*desktopauth.Session, error) {
	sess, err := login.Start()
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	fmt.Println("已打开小米登录窗口，请在该窗口里完成登录（5 分钟超时）。")
	fmt.Println("本工具只读取该窗口自己的 Cookie，不会读取 MiMo 的任何数据。")
	ctx, cancel := context.WithTimeout(context.Background(), login.DefaultTimeout)
	defer cancel()
	return sess.Wait(ctx)
}

// cmdLogin is the supported way to obtain a desktop session: the user signs in to Xiaomi
// passport in a window this tool owns, so MiMo never has to run or be inspected.
func cmdLogin() error {
	session, err := signIn()
	if err != nil {
		return err
	}
	return adopt(session, "登录成功，桌面端会话已保存")
}

// adopt verifies a desktop session against the models MiMo currently advertises, rather than
// a hardcoded list, and stores it as the live credential.
func adopt(session *desktopauth.Session, done string) error {
	catalog, _ := usage.LoadTextModels()
	candidates := make([]string, 0, len(catalog))
	for _, m := range catalog {
		candidates = append(candidates, m.ID)
	}
	model, err := session.Verify(candidates...)
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	cfg.SetCredential(&store.Credential{
		Kind:      store.KindDesktop,
		Cookie:    session.CookieHeader(),
		BaseURL:   desktopauth.BaseURL,
		UID:       session.UserID,
		Model:     model,
		PassToken: session.PassToken,
		CUserId:   session.CUserId,
		IssuedAt:  session.HarvestedAt.UTC(),
	})
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Println(done + "。")
	fmt.Printf("  上游      : %s\n", desktopauth.BaseURL)
	fmt.Printf("  可用模型  : %s\n", model)
	fmt.Printf("  凭证      : %s\n", cfg.Credential().Masked())
	if session.PassToken != "" {
		fmt.Println("  passToken : 已保存，可用于日后自行续期")
	}
	return nil
}
func cmdAutostart(args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "on":
		if err := autostart.Enable(); err != nil {
			return err
		}
		fmt.Println("已开启开机静默自启（托盘模式，不显示窗口）。")
		return nil
	case "off":
		if err := autostart.Disable(); err != nil {
			return err
		}
		fmt.Println("已关闭开机自启。")
		return nil
	default:
		on, command, err := autostart.Enabled()
		if err != nil {
			return err
		}
		if !on {
			fmt.Println("开机自启: 未开启")
			return nil
		}
		fmt.Println("开机自启: 已开启")
		fmt.Println("  启动命令:", command)
		return nil
	}
}

func cmdRefresh() error {
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	if cfg.Credential() == nil {
		return fmt.Errorf("还没有凭证，先运行: mimo-switch login")
	}
	if cfg.Credential().PassToken == "" {
		return fmt.Errorf("当前凭证没有保存 passToken，无法自动续期。请重新运行: mimo-switch login")
	}
	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	before := cfg.Credential().Fingerprint()
	if err := srv.RefreshNow(); err != nil {
		return err
	}
	cred := cfg.Credential()
	after := cred.Fingerprint()
	fmt.Println("续期完成。")
	fmt.Printf("  指纹   : %s -> %s\n", before, after)
	if before == after {
		fmt.Println("  说明   : 服务端返回了同一个 serviceToken（尚未轮换）")
	} else {
		fmt.Println("  说明   : 已换成新的 serviceToken")
	}
	fmt.Printf("  模型   : %s\n", cred.Model)
	fmt.Printf("  时间   : %s\n", cred.IssuedAt.Local().Format(time.RFC3339))
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "", "覆盖监听地址")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cred := cfg.Credential()
	fmt.Printf("本地端点 http://%s/v1\n", cfg.Listen)
	fmt.Printf("上游     %s  套餐 %s  key %s\n", cred.BaseURL, cred.Plan(), cred.Masked())
	if cfg.RequireToken {
		fmt.Printf("本地 token %s\n", cfg.LocalToken)
	}
	fmt.Println("Ctrl-C 退出")
	return srv.ListenAndServe(ctx)
}
