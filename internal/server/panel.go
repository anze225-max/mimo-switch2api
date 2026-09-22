package server

import (
	_ "embed"
	"net/http"
	"time"

	"mimo-switch/internal/quota"
	"mimo-switch/internal/store"
	"mimo-switch/internal/usage"
)

//go:embed assets/panel.html
var panelHTML []byte

// planLabel localises the quota source for the panel.
var planLabel = map[string]string{
	"desktop-free": "MiMo 桌面端免费额度",
	"token-plan":   "Token Plan 订阅",
	"billing":      "平台计费额度",
}

func (s *Server) handlePanel(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(panelHTML)
}

// handleStatus backs the panel. It is loopback-only like everything else, and it is the
// one route that does not require the bearer token so the user can open it in a browser
// and read the credential they need to paste into other tools.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) || !isLocalHost(r.Host) {
		writeError(w, http.StatusForbidden, "forbidden", "only loopback clients are allowed")
		return
	}
	snap := s.tracker.Snapshot()
	desktop := s.credential().Kind == store.KindDesktop
	label := planLabel[s.credential().Plan()]
	if label == "" {
		label = s.credential().Plan()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"healthy":       true,
		"plan":          s.credential().Plan(),
		"plan_label":    label,
		"desktop":       desktop,
		"upstream":      s.credential().BaseURL,
		"model":         s.modelOrDefault(),
		"listen":        s.listen,
		"require_token": s.requireToken,
		"local_token":   s.localToken,
		"credential":    s.credential().Masked(),
		"issued_at":     s.credential().IssuedAt,
		"uptime_s":      int(time.Since(s.startedAt).Seconds()),
		"usage":         snap,
		"models":        s.panelModels(),
		"quota":         s.quotaStatus(),
	})
}

// quotaStatus returns the live desktop allowance, cached briefly so refreshing the panel
// cannot hammer MiMo's endpoint. A failure is reported as text rather than hidden, because
// a missing number is itself information the user needs.
func (s *Server) quotaStatus() map[string]any {
	if s.credential().Kind != store.KindDesktop {
		return map[string]any{"available": false, "note": "非桌面端会话，无此额度接口"}
	}

	// The upstream call can take seconds, so it runs outside the lock and only ever once at
	// a time. A panel refresh that arrives mid-fetch gets the cached figure instead of
	// queueing behind it, which used to freeze the page.
	s.quotaMu.Lock()
	cached := s.quotaValue
	fresh := cached != nil && time.Since(s.quotaAt) < quotaTTL
	startFetch := !fresh && !s.quotaFetching
	if startFetch {
		s.quotaFetching = true
	}
	s.quotaMu.Unlock()

	if fresh || !startFetch {
		if cached == nil {
			return map[string]any{"available": false, "note": "额度查询进行中…"}
		}
		return quotaJSON(cached, "")
	}
	defer func() {
		s.quotaMu.Lock()
		s.quotaFetching = false
		s.quotaMu.Unlock()
	}()

	cred := s.credential()
	u, err := quota.Fetch(cred.BaseURL, cred.Cookie)
	if err != nil {
		if cached != nil {
			return quotaJSON(cached, err.Error())
		}
		return map[string]any{"available": false, "note": err.Error()}
	}
	s.quotaMu.Lock()
	s.quotaValue, s.quotaAt = u, time.Now()
	s.quotaMu.Unlock()
	return quotaJSON(u, "")
}

func quotaJSON(u *quota.Usage, note string) map[string]any {
	out := map[string]any{
		"available": true, "remaining_percent": u.RemainingPercent, "reset_date": u.ResetDate,
	}
	if note != "" {
		out["note"] = note
	}
	return out
}

// panelModels reports the live catalogue with ratios, so the panel reflects a MiMo update
// without a code change.
func (s *Server) panelModels() []usage.Model {
	out := make([]usage.Model, 0, len(s.catalogue()))
	for _, m := range s.catalogue() {
		out = append(out, usage.Model{ID: m.ID, Multiplier: m.Ratio})
	}
	return out
}

// modelOrDefault keeps the panel's copy-paste snippets usable even before a harvest has
// pinned a specific model.
func (s *Server) modelOrDefault() string {
	if s.credential().Model != "" {
		return s.credential().Model
	}
	return usage.PreferredModel(s.catalogue())
}
