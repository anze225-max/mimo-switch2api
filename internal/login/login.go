// Package login obtains the MiMo desktop session the way the desktop app itself does: it
// opens Xiaomi's passport page in a throwaway browser window, waits for the user to sign in
// there, and keeps the passToken that page sets. No other application's storage is read.
package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"mimo-switch/internal/cdp"
	"mimo-switch/internal/desktopauth"
)

// loginURL asks passport for a MiMo-desktop session directly. MiMo's own jar keeps its
// serviceToken on .mimo-server-cn.xiaomimimo.com, so the redirect chain started from here can
// leave us that token outright; asking for the passport sid instead yields a token for the
// wrong service and the later exchange then fails with 70016.
const loginURL = "https://account.xiaomi.com/pass/serviceLogin?sid=mimopc&_locale=zh_CN"

// DefaultTimeout is generous: signing in usually means fumbling for a phone to scan a QR.
const DefaultTimeout = 5 * time.Minute

const pollEvery = 1500 * time.Millisecond

// ErrWindowClosed means the user dismissed the browser before signing in.
var ErrWindowClosed = errors.New("登录窗口已关闭，未完成登录")

// cookieURLs deliberately includes the /pass/auth path: passport scopes userId and cUserId
// there, and getCookies filters by path, so a site-root query would miss them.
var cookieURLs = []string{
	"https://account.xiaomi.com/pass/serviceLogin",
	"https://account.xiaomi.com/",
	"https://mimo-server-cn.xiaomimimo.com/",
}

type Session struct {
	profile string
	conn    *cdp.Conn
	cmd     *exec.Cmd
	exited  chan struct{}
}

// Start opens the login window. It returns once the window's debug endpoint answers, which
// is before the user has done anything — call Wait for that.
func Start() (*Session, error) {
	exe, err := browserPath()
	if err != nil {
		return nil, err
	}
	profile, err := os.MkdirTemp("", "mimo-switch-login-")
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		os.RemoveAll(profile)
		return nil, err
	}
	cmd, err := openLoginWindow(exe, profile, port, loginURL)
	if err != nil {
		os.RemoveAll(profile)
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	s := &Session{profile: profile, cmd: cmd, exited: exited}
	conn, err := waitForDebugger(port, 30*time.Second)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	s.conn = conn
	return s, nil
}

// windowSession is what the sign-in window holds once the redirect chain has run.
//
// MiMo's own cookie jar keeps the serviceToken on .mimo-server-cn.xiaomimimo.com, so when
// the login page completes that redirect the token is already in the window and no passport
// exchange is needed. The exchange is only the fallback.
type windowSession struct {
	ServiceToken string
	Master       desktopauth.MasterCredential
}

// Wait blocks until the window holds a passport session, then returns a desktop session.
func (s *Session) Wait(ctx context.Context) (*desktopauth.Session, error) {
	var lastErr error
	for {
		win, err := s.windowSession()
		switch {
		case errors.Is(err, ErrWindowClosed):
			return nil, err
		case err != nil:
			lastErr = err
		case win.ServiceToken != "":
			return &desktopauth.Session{
				ServiceToken: win.ServiceToken,
				UserID:       win.Master.UserID,
				CUserId:      win.Master.CUserId,
				PassToken:    win.Master.PassToken,
				HarvestedAt:  time.Now().UTC(),
			}, nil
		case win.Master.PassToken != "" && win.Master.UserID != "":
			session, err := desktopauth.Refresh(win.Master)
			if err != nil {
				// The cookie names (never values) are what distinguishes "logged in" from
				// "logged in against the wrong service", so report them.
				return nil, fmt.Errorf("%w（窗口里的 Cookie：%s）", err, jarNames(win.Master.SessionCookies))
			}
			return session, nil
		default:
			lastErr = nil // a clean read with no passToken just means: still waiting
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, fmt.Errorf("登录超时: %w（最后一次读取 Cookie 失败：%v）", ctx.Err(), lastErr)
			}
			return nil, fmt.Errorf("登录超时: %w", ctx.Err())
		case <-s.exited:
			return nil, ErrWindowClosed
		case <-time.After(pollEvery):
		}
	}
}

// Close shuts the window and wipes the profile that held its cookies.
func (s *Session) Close() error {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if s.cmd != nil {
		killTree(s.cmd)
		<-s.exited
	}
	return removeProfile(s.profile)
}

func (s *Session) windowSession() (*windowSession, error) {
	select {
	case <-s.exited:
		return nil, ErrWindowClosed
	default:
	}
	raw, err := s.conn.Call("Network.getCookies", map[string]any{"urls": cookieURLs})
	if err != nil {
		return nil, err
	}
	return parseWindowCookies(raw)
}

// parseWindowCookies reads what the sign-in window ended up with. Only the serviceToken
// served to MiMo's own host counts; a passport-scoped one is a different service's ticket.
func parseWindowCookies(raw json.RawMessage) (*windowSession, error) {
	var reply struct {
		Cookies []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("解析登录窗口的 Cookie: %w", err)
	}
	win := &windowSession{Master: desktopauth.MasterCredential{SID: desktopauth.DefaultSID}}
	jar := map[string]string{}
	for _, c := range reply.Cookies {
		v := strings.TrimSpace(c.Value)
		if v == "" {
			continue
		}
		jar[c.Name] = v // the window's latest value for a name wins
		switch c.Name {
		case "passToken":
			win.Master.PassToken = v
		case "userId":
			if win.Master.UserID == "" {
				win.Master.UserID = v
			}
		case "cUserId":
			if win.Master.CUserId == "" {
				win.Master.CUserId = v
			}
		case "serviceToken":
			if strings.Contains(c.Domain, "mimo-server") {
				win.ServiceToken = v
			}
		}
	}
	parts := make([]string, 0, len(jar))
	for name, v := range jar {
		parts = append(parts, name+"="+v)
	}
	sort.Strings(parts)
	win.Master.SessionCookies = strings.Join(parts, "; ")
	return win, nil
}

// removeProfile retries: the renderers we just killed can hold the directory for a moment,
// and leaving it behind would strand a signed-in Xiaomi session on disk.
func removeProfile(dir string) error {
	if dir == "" {
		return nil
	}
	var err error
	for i := 0; i < 10; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("临时登录资料没能删除，请手动清理 %s: %w", dir, err)
}

func waitForDebugger(port int, timeout time.Duration) (*cdp.Conn, error) {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := cdp.Connect(port)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("登录窗口的调试端口没有就绪: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("取空闲端口: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// jarNames lists cookie names from a Cookie header, for diagnostics only: values never
// reach the log.
func jarNames(header string) string {
	var names []string
	for _, part := range strings.Split(header, "; ") {
		if i := strings.IndexByte(part, '='); i > 0 {
			names = append(names, part[:i])
		}
	}
	return strings.Join(names, ",")
}
