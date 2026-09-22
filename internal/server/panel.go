package server

import (
	_ "embed"
	"net/http"
	"time"

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
	desktop := s.credential.Kind == store.KindDesktop
	label := planLabel[s.credential.Plan()]
	if label == "" {
		label = s.credential.Plan()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"healthy":       true,
		"plan":          s.credential.Plan(),
		"plan_label":    label,
		"desktop":       desktop,
		"upstream":      s.credential.BaseURL,
		"model":         s.modelOrDefault(),
		"listen":        s.listen,
		"require_token": s.requireToken,
		"local_token":   s.localToken,
		"credential":    s.credential.Masked(),
		"issued_at":     s.credential.IssuedAt,
		"uptime_s":      int(time.Since(s.startedAt).Seconds()),
		"usage":         snap,
		"models":        s.panelModels(),
	})
}

// panelModels reports the live catalogue with ratios, so the panel reflects a MiMo update
// without a code change.
func (s *Server) panelModels() []usage.Model {
	out := make([]usage.Model, 0, len(s.models))
	for _, m := range s.models {
		out = append(out, usage.Model{ID: m.ID, Multiplier: m.Ratio})
	}
	return out
}

// modelOrDefault keeps the panel's copy-paste snippets usable even before a harvest has
// pinned a specific model.
func (s *Server) modelOrDefault() string {
	if s.credential.Model != "" {
		return s.credential.Model
	}
	return usage.PreferredModel(s.models)
}
