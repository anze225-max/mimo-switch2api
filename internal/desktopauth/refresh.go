package desktopauth

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Xiaomi passport SSO, transcribed from the desktop app's ServiceTokenManager so the tool
// can renew serviceToken itself once the desktop session goes stale.
const (
	passportStep1 = "https://account.xiaomi.com/pass/serviceLogin"
	// DefaultSID is the service id MiMo desktop registers with passport; the login surface
	// observed on this machine used "mimopc".
	DefaultSID    = "mimopc"
	passportUA    = "MiClaw/1.0"
	jsonHijackPfx = "&&&START&&&"
)

// MasterCredential is the long-lived half of the desktop login: what survives a restart and
// lets us mint a fresh serviceToken.
type MasterCredential struct {
	PassToken string
	UserID    string
	CUserId   string
	SID       string
}

func (m MasterCredential) cookieHeader() string {
	parts := []string{"userId=" + m.UserID, "passToken=" + m.PassToken}
	if m.CUserId != "" {
		parts = append(parts, "cUserId="+m.CUserId)
	}
	return strings.Join(parts, "; ")
}

type phase1Response struct {
	Code             int    `json:"code"`
	Location         string `json:"location"`
	Security         string `json:"ssecurity"`
	Nonce            json.Number
	SecondValidation bool   `json:"secondValidation"`
	NotificationURL  string `json:"notificationUrl"`
}

// ClientSign is urlencode(base64(sha1("nonce=<nonce>&<ssecurity>"))). It is a bare SHA-1
// over a concatenation, not an HMAC, and the nonce must be the decimal string form.
func ClientSign(nonce, ssecurity string) string {
	input := "nonce=" + nonce
	if strings.TrimSpace(ssecurity) != "" {
		input += "&" + ssecurity
	}
	sum := sha1.Sum([]byte(input))
	return url.QueryEscape(base64.StdEncoding.EncodeToString(sum[:]))
}

// Refresh exchanges the master credential for a live serviceToken.
func Refresh(m MasterCredential) (*Session, error) {
	if m.PassToken == "" || m.UserID == "" {
		return nil, fmt.Errorf("缺少 passToken 或 userId，无法续期")
	}
	if m.SID == "" {
		m.SID = DefaultSID
	}
	client := &http.Client{Timeout: 25 * time.Second, Jar: newCookieJar()}

	first, err := http.NewRequest(http.MethodGet, step1URL(m.SID), nil)
	if err != nil {
		return nil, err
	}
	setPassportHeaders(first, m)
	firstRes, err := client.Do(first)
	if err != nil {
		return nil, fmt.Errorf("Phase 1 请求失败: %w", err)
	}
	defer firstRes.Body.Close()
	body, err := io.ReadAll(io.LimitReader(firstRes.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	phase1, err := parsePhase1(string(body))
	if err != nil {
		return nil, err
	}
	if phase1.Code != 0 {
		return nil, fmt.Errorf("Phase 1 返回 code=%d（passToken 可能已失效，需要在 MiMo 重新登录）", phase1.Code)
	}
	if phase1.SecondValidation && phase1.NotificationURL != "" {
		return nil, fmt.Errorf("小米要求二次验证，请在 MiMo 桌面端手动登录后重新 harvest：%s", phase1.NotificationURL)
	}
	if phase1.Location == "" {
		return nil, fmt.Errorf("Phase 1 未返回跳转地址")
	}

	secondURL := phase1.Location + "&clientSign=" + ClientSign(phase1.Nonce.String(), phase1.Security)
	second, err := http.NewRequest(http.MethodGet, secondURL, nil)
	if err != nil {
		return nil, err
	}
	setPassportHeaders(second, m)
	secondRes, err := client.Do(second)
	if err != nil {
		return nil, fmt.Errorf("Phase 2 请求失败: %w", err)
	}
	defer secondRes.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(secondRes.Body, 1<<20))

	serviceToken := ""
	for _, c := range secondRes.Cookies() {
		if c.Name == "serviceToken" {
			serviceToken = c.Value
		}
	}
	if serviceToken == "" {
		// Passport may have set it only on the jar.
		if loc, err := url.Parse(phase1.Location); err == nil {
			for _, c := range client.Jar.Cookies(loc) {
				if c.Name == "serviceToken" {
					serviceToken = c.Value
				}
			}
		}
	}
	if serviceToken == "" {
		return nil, fmt.Errorf("Phase 2 未返回 serviceToken（HTTP %d）", secondRes.StatusCode)
	}

	return &Session{
		ServiceToken: serviceToken,
		UserID:       m.UserID,
		PassToken:    m.PassToken,
		HarvestedAt:  time.Now(),
	}, nil
}

func step1URL(sid string) string {
	q := url.Values{}
	q.Set("_locale", "zh_CN")
	q.Set("_snsNone", "true")
	q.Set("sid", sid)
	q.Set("_json", "true")
	return passportStep1 + "?" + q.Encode()
}

// parsePhase1 strips passport's anti-JSON-hijacking prefix and coerces nonce to a string,
// matching the desktop app: the signature uses the decimal text, and JSON would otherwise
// round-trip it as a float.
func parsePhase1(body string) (*phase1Response, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(body, jsonHijackPfx))
	nonce := ""
	if m := regexp.MustCompile(`"nonce"\s*:\s*(\d+)`).FindStringSubmatch(trimmed); m != nil {
		nonce = m[1]
	}
	var out phase1Response
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("Phase 1 响应解析失败: %w (%s)", err, trimPrefix(trimmed, 120))
	}
	if nonce != "" {
		out.Nonce = json.Number(nonce)
	}
	return &out, nil
}

func trimPrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// setPassportHeaders applies the exact headers the desktop app sends to passport.
func setPassportHeaders(req *http.Request, m MasterCredential) {
	req.Header.Set("Cookie", m.cookieHeader())
	req.Header.Set("User-Agent", passportUA)
}

// newCookieJar keeps the session cookie passport issues during Phase 1 so Phase 2 is seen
// as the same login continuation.
func newCookieJar() http.CookieJar {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil
	}
	return jar
}
