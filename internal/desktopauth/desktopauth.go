// Package desktopauth harvests and replays the MiMo desktop app's free-quota session.
//
// The desktop authenticates to mimo-server with a `serviceToken` cookie minted from the
// Xiaomi `passToken` (which lasts ~30 days). Reading those two cookies out of the running
// app's login partition is enough to call the OpenAI-compatible endpoint on its own, so
// MiMo does not have to stay open afterwards.
package desktopauth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mimo-switch/internal/cdp"
)

// Endpoint the desktop's own chat traffic uses. It speaks OpenAI's chat/completions shape.
const (
	BaseURL    = "https://mimo-server-cn.xiaomimimo.com/api/route"
	ExportName = "xiaomi-session-cookies.json"
)

// Session is the replayable credential.
type Session struct {
	ServiceToken string
	UserID       string
	CUserId      string
	PassToken    string
	Model        string
	HarvestedAt  time.Time
}

// Master returns the long-lived half, which is enough to mint a new serviceToken later.
func (s *Session) Master() MasterCredential {
	return MasterCredential{
		PassToken: s.PassToken,
		UserID:    s.UserID,
		CUserId:   s.CUserId,
		SID:       DefaultSID,
	}
}

// HasMaster reports whether silent renewal is possible with this credential.
func (s *Session) HasMaster() bool {
	return s.PassToken != "" && s.UserID != ""
}

// CookieHeader is what mimo-server actually authenticates with.
func (s *Session) CookieHeader() string {
	parts := []string{"serviceToken=" + s.ServiceToken}
	if s.UserID != "" {
		parts = append(parts, "userId="+s.UserID)
	}
	return strings.Join(parts, "; ")
}

type chromiumCookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Expires float64 `json:"expirationDate"`
}

// LoadExport reads the cookie export produced by `mimo-switch harvest`.
func LoadExport(path string) (*Session, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 cookie 导出: %w", err)
	}
	return ParseExport(raw)
}

// ParseExport turns a Chromium cookie dump into a replayable session.
func ParseExport(raw []byte) (*Session, error) {
	var cookies []chromiumCookie
	if err := json.Unmarshal(raw, &cookies); err != nil {
		return nil, fmt.Errorf("cookie 导出格式: %w", err)
	}
	s := &Session{HarvestedAt: time.Now()}
	for _, c := range cookies {
		switch c.Name {
		case "serviceToken":
			if strings.Contains(c.Domain, "mimo-server") {
				s.ServiceToken = c.Value
			}
		case "userId":
			// The mimo-server pair is the one that matters; account.xiaomi.com is only a fallback.
			if s.UserID == "" || strings.Contains(c.Domain, "xiaomimimo") {
				if strings.Contains(c.Domain, "xiaomimimo") {
					s.UserID = c.Value
				} else if s.UserID == "" {
					s.UserID = c.Value
				}
			}
		case "passToken":
			s.PassToken = c.Value
		case "cUserId":
			// Passport echoes this back on the serviceLogin call.
			if s.CUserId == "" {
				s.CUserId = c.Value
			}
		}
	}
	if s.ServiceToken == "" {
		return nil, fmt.Errorf("导出里没有 serviceToken（MiMo 是否已登录？）")
	}
	return s, nil
}

// cookieSnippet runs inside MiMo's main process to read its own login partition. The
// serviceToken is a session cookie, so it only exists in the live jar, never on disk.
const cookieSnippet = `
const { session } = require("electron");
const cookies = await session.fromPartition("persist:xiaomi-account").cookies.get({});
return JSON.stringify(cookies);
`

// HarvestLive reads the session out of a running MiMo that exposes a debug port.
func HarvestLive(port int) (*Session, string, error) {
	conn, err := cdp.Connect(port)
	if err != nil {
		return nil, "", err
	}
	defer conn.Close()

	raw, err := conn.Evaluate(cookieSnippet)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(raw) == "" || strings.HasPrefix(raw, "[]") {
		return nil, "", fmt.Errorf("登录分区里没有 cookie（MiMo 已登录了吗？）")
	}
	session, err := ParseExport([]byte(raw))
	if err != nil {
		return nil, "", err
	}
	if err := writeExport(raw); err != nil {
		return nil, "", err
	}
	return session, raw, nil
}

// writeExport keeps the raw jar for debugging and for re-verifying without MiMo running.
func writeExport(raw string) error {
	path, err := ExportPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(raw), 0o600)
}

// Verify confirms the session really reaches the model and reports which one worked.
// Candidates come from MiMo's live catalogue, since model ids change with each generation.
func (s *Session) Verify(candidates ...string) (string, error) {
	if len(candidates) == 0 {
		candidates = []string{"mimo-v2.6-pro", "mimo-v2.6-flash"}
	}
	client := &http.Client{Timeout: 40 * time.Second}
	var lastErr error
	for _, model := range candidates {
		payload := `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"Reply with exactly: OK"}]}`
		req, err := http.NewRequest(http.MethodPost, BaseURL+"/chat/completions", strings.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("cookie", s.CookieHeader())
		res, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 600))
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			s.Model = model
			return model, nil
		}
		lastErr = fmt.Errorf("%s -> HTTP %d %s", model, res.StatusCode, trim(string(body)))
		// A non-auth failure means this model is wrong, not the session; keep trying.
		if res.StatusCode != http.StatusNotFound && res.StatusCode != http.StatusBadRequest {
			return "", lastErr
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用模型")
	}
	return "", fmt.Errorf("会话未被任何模型接受（serviceToken 可能已过期，请在 MiMo 重新登录后 harvest）：%w", lastErr)
}

// ExportPath is the default drop location for a harvest run.
func ExportPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "mimo-switch", ExportName), nil
}

func trim(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
