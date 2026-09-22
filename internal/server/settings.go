package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"mimo-switch/internal/autostart"
)

// keepaliveLoop wakes once a minute and renews the desktop session once the configured
// interval has elapsed, so client traffic almost never sees a 401. The interval is re-read
// every pass, which lets a change from the panel take effect without a restart.
func (s *Server) keepaliveLoop(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			interval, ok := s.keepalive()
			if !ok || !s.canRefresh() {
				continue
			}
			if time.Since(s.lastRenewal()) < interval {
				continue
			}
			if err := s.refreshSession(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "保活续期失败: %v\n", err)
			}
		}
	}
}

func (s *Server) keepalive() (time.Duration, bool) {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	if s.keepMinutes <= 0 {
		return 0, false
	}
	return time.Duration(s.keepMinutes) * time.Minute, true
}

// AutoRestart reports whether the listener should be retried after it dies.
func (s *Server) AutoRestart() bool {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.autoRestart
}

// lastRenewal is when the session was last swapped in. A server that has never renewed counts
// as renewed at startup, so the timer waits a full interval instead of firing immediately.
func (s *Server) lastRenewal() time.Time {
	s.refreshLock.Lock()
	defer s.refreshLock.Unlock()
	if s.refreshedAt.IsZero() {
		return s.startedAt
	}
	return s.refreshedAt
}

// NoteRecovery records one automatic listener restart for the panel to surface.
func (s *Server) NoteRecovery(err error) {
	s.setMu.Lock()
	s.recovered++
	s.setMu.Unlock()
	fmt.Fprintf(os.Stderr, "本地端点中断，已自动重启: %v\n", err)
}

// Recoveries is how many times the listener has been restarted since startup.
func (s *Server) Recoveries() int64 {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.recovered
}

// settings is the subset of the configuration the user may change at runtime.
type settings struct {
	KeepaliveMinutes int  `json:"keepalive_minutes"`
	RestartOnCrash   bool `json:"restart_on_crash"`
}

func (s *Server) currentSettings() settings {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return settings{KeepaliveMinutes: s.keepMinutes, RestartOnCrash: s.autoRestart}
}

// handleSettings applies panel changes and persists them. cfg is nil in tests, where the
// values still apply to this process but nothing reaches the disk.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		KeepaliveMinutes *int  `json:"keepalive_minutes"`
		RestartOnCrash   *bool `json:"restart_on_crash"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if patch.KeepaliveMinutes == nil && patch.RestartOnCrash == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "没有要修改的设置项")
		return
	}
	if patch.KeepaliveMinutes != nil && (*patch.KeepaliveMinutes < 0 || *patch.KeepaliveMinutes > 7*24*60) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "保活间隔需为 0（关闭）到 10080 分钟之间")
		return
	}

	s.setMu.Lock()
	if patch.KeepaliveMinutes != nil {
		s.keepMinutes = *patch.KeepaliveMinutes
	}
	if patch.RestartOnCrash != nil {
		s.autoRestart = *patch.RestartOnCrash
	}
	current := settings{KeepaliveMinutes: s.keepMinutes, RestartOnCrash: s.autoRestart}
	s.setMu.Unlock()

	if s.cfg != nil {
		s.cfg.KeepaliveMinutes = current.KeepaliveMinutes
		restart := current.RestartOnCrash
		s.cfg.RestartOnCrash = &restart
		if err := s.cfg.Save(); err != nil {
			writeError(w, http.StatusInternalServerError, "api_error", "设置已生效但保存失败: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, current)
}

// SetShutdown lets the resident app register a way to stop itself, so the uninstaller can
// ask the running instance to exit before deleting files out from under it.
func (s *Server) SetShutdown(f func()) {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	s.shutdownFn = f
}

// handleShutdown stops this process. The reply goes out first so the caller learns it
// landed; the guard means only a token-holder on loopback can pull the plug.
func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	s.setMu.Lock()
	f := s.shutdownFn
	s.setMu.Unlock()
	if f == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "这个进程没有注册退出方式")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "正在退出"})
	if ctl, ok := w.(http.Flusher); ok {
		ctl.Flush()
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		f()
	}()
}

// autostartState reports whether logon will launch the tool, so the panel can show and
// change it without the user having to open a terminal.
func (s *Server) autostartState() map[string]any {
	on, command, err := autostart.Enabled()
	state := map[string]any{"enabled": on, "command": command}
	if err != nil {
		state["error"] = err.Error()
	}
	return state
}

// handleAutostart writes the per-user Run key. Guarded like every other POST: it changes
// what happens when the machine boots.
func (s *Server) handleAutostart(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		Enabled *bool `json:"enabled"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &patch); err != nil || patch.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "需要 {\"enabled\": true/false}")
		return
	}
	if *patch.Enabled {
		err = autostart.Enable()
	} else {
		err = autostart.Disable()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.autostartState())
}
