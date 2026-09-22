// Package quota reads the remaining desktop allowance from MiMo's own usage endpoint.
package quota

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Path is the endpoint the desktop app's "剩余用量" menu item is backed by.
const Path = "/user/usage"

// Usage is the decoded response.
//
// RemainingPercent is the *remaining* share of the allowance, not the used share. This was
// confirmed against the desktop UI on this account (API 95.8 == UI "剩余 95.8%"), so do not
// invert it.
type Usage struct {
	RemainingPercent float64   `json:"remaining_percent"`
	ResetDate        string    `json:"reset_date,omitempty"`
	ResetAt          time.Time `json:"reset_at,omitempty"`

	raw struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			// Percent is a pointer so an absent field cannot read as "0% left", which the
			// panel would render as a spent allowance.
			Percent    *float64 `json:"percent"`
			ResetDate  string   `json:"resetDate"`
			ResetAtSec int64    `json:"resetAt"`
		} `json:"data"`
	}
}

// Fetch reads the allowance using the desktop session cookie.
func Fetch(baseURL, cookie string) (*Usage, error) {
	url := strings.TrimRight(baseURL, "/")
	// The usage endpoint lives beside the chat route, under the same /api prefix.
	url = strings.TrimSuffix(url, "/route") + Path
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("cookie", cookie)
	req.Header.Set("user-agent", "mimocode/desktop-5198ff5")

	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求额度: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("额度接口 HTTP %d %s", res.StatusCode, trim(string(body)))
	}

	var u Usage
	if err := json.Unmarshal(body, &u.raw); err != nil {
		return nil, fmt.Errorf("额度接口响应解析失败: %w (%s)", err, trim(string(body)))
	}
	if u.raw.Code != 0 {
		return nil, fmt.Errorf("额度接口返回 code=%d %s", u.raw.Code, u.raw.Message)
	}
	if u.raw.Data.Percent == nil {
		return nil, fmt.Errorf("额度接口没有给出 percent (%s)", trim(string(body)))
	}
	u.RemainingPercent = *u.raw.Data.Percent
	u.ResetDate = u.raw.Data.ResetDate
	if u.raw.Data.ResetAtSec > 0 {
		u.ResetAt = time.Unix(u.raw.Data.ResetAtSec, 0)
	}
	return &u, nil
}

func trim(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
