package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"mimo-switch/internal/auth"
	"mimo-switch/internal/autostart"
	"mimo-switch/internal/desktopauth"
	"mimo-switch/internal/login"
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
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `mimo-switch — 把 MiMo 免费额度暴露成本地 OpenAI/Anthropic 端点

用法:
  mimo-switch login                在本工具打开的小米登录窗口里登录，取回桌面端免费额度（推荐）
  mimo-switch refresh              用已存的 passToken 静默续期（无需 MiMo 运行）
  mimo-switch serve                启动本地反代
  mimo-switch tray                 后台运行 + 托盘图标（静默，无控制台）
  mimo-switch autostart on|off     开机静默自启
  mimo-switch status               查看已保存的凭证（掩码显示）
  mimo-switch models               用当前凭证拉取上游模型列表，验证可用
  mimo-switch probe [model]        发一条最小补全，验证端到端

  其它:
  mimo-switch authorize            打开官方授权页签发 API key（需小米付费套餐）
  mimo-switch authorize --code X   授权页显示 code 时的手动兜底
  mimo-switch harvest [--file F]   从正在运行的 MiMo 取会话（旧办法，login 不可用时的兜底）
  mimo-switch harvest --launch     未运行时自动带调试端口拉起 MiMo 并等待登录
`)
}

func run(args []string) error {
	if len(args) == 0 {
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
		return tray.Run()
	case "autostart":
		return cmdAutostart(args[1:])
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
		return nil, nil, fmt.Errorf("还没有凭证，先运行: mimo-switch authorize")
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
		if !desktopauth.Running() {
			if err := desktopauth.LaunchMiMo(*exe, 9229); err != nil {
				return err
			}
			fmt.Println("已启动 MiMo，请在其窗口里用小米账号登录。")
		} else {
			fmt.Println("MiMo 已在运行；若它不是以调试端口启动的，请先完全退出再试 --launch。")
		}
		if err := desktopauth.WaitReady(9229, 90*time.Second); err != nil {
			return err
		}
		fmt.Println("等待你在 MiMo 窗口中完成登录…（最多 5 分钟）")
		deadline := time.Now().Add(5 * time.Minute)
		for {
			s, _, err := desktopauth.HarvestLive(9229)
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
				return fmt.Errorf("%w\n（也可先导出 cookie 再用 --file 指定；或让 MiMo 以 --remote-debugging-port=9229 启动）", lastErr)
			}
			return fmt.Errorf("未能取回会话")
		}
	}

	return adopt(session, "已接管 MiMo 桌面端免费额度会话")
}

// cmdLogin is the supported way to obtain a desktop session: the user signs in to Xiaomi
// passport in a window this tool owns, so MiMo never has to run or be inspected.
func cmdLogin() error {
	sess, err := login.Start()
	if err != nil {
		return err
	}
	defer sess.Close()

	fmt.Println("已打开小米登录窗口，请在该窗口里完成登录（5 分钟超时）。")
	fmt.Println("本工具只读取该窗口自己的 Cookie，不会读取 MiMo 的任何数据。")
	ctx, cancel := context.WithTimeout(context.Background(), login.DefaultTimeout)
	defer cancel()
	session, err := sess.Wait(ctx)
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
		return fmt.Errorf("还没有凭证，先运行: mimo-switch harvest")
	}
	if cfg.Credential().PassToken == "" {
		return fmt.Errorf("当前凭证没有保存 passToken，无法自动续期。请重新运行: mimo-switch harvest")
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
