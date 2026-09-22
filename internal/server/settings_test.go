package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func settingsRequest(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	s.localToken = "secret-token"
	s.requireToken = true
	req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:7864"
	req.Header.Set("authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSettingsRoundTrip(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	s := newTestServer(upstreamSrv.URL, desktopCred(), nil)

	rec := settingsRequest(t, s, `{"keepalive_minutes":120,"restart_on_crash":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := s.currentSettings(); got.KeepaliveMinutes != 120 || got.RestartOnCrash {
		t.Errorf("settings = %+v, want 120 minutes and auto-restart off", got)
	}
	if d, ok := s.keepalive(); !ok || d != 2*time.Hour {
		t.Errorf("keepalive = %v,%v want 2h,true", d, ok)
	}
	if s.AutoRestart() {
		t.Error("AutoRestart = true, want the panel's off to stick")
	}
}

// 0 is a real value: it switches the timer off rather than falling back to the default.
func TestSettingsZeroTurnsKeepaliveOff(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	s := newTestServer(upstreamSrv.URL, desktopCred(), nil)
	if d, ok := s.keepalive(); !ok || d == 0 {
		t.Fatalf("fresh server should default to a running timer, got %v,%v", d, ok)
	}
	settingsRequest(t, s, `{"keepalive_minutes":0}`)
	if d, ok := s.keepalive(); ok || d != 0 {
		t.Errorf("keepalive = %v,%v want 0,false after switching off", d, ok)
	}
}

func TestSettingsRejectsNonsense(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	s := newTestServer(upstreamSrv.URL, desktopCred(), nil)

	before := s.currentSettings()
	for _, body := range []string{`{}`, `{"keepalive_minutes":-1}`, `{"keepalive_minutes":99999}`, `not json`} {
		rec := settingsRequest(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s -> %s, want 400", body, rec.Body.String())
		}
	}
	if got := s.currentSettings(); got != before {
		t.Errorf("rejected writes changed settings to %+v", got)
	}
}

func TestSettingsRequiresTheLocalToken(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	s := newTestServer(upstreamSrv.URL, desktopCred(), nil)
	s.localToken = "secret-token"
	s.requireToken = true

	req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"keepalive_minutes":1}`))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost:7864"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := s.currentSettings(); got.KeepaliveMinutes == 1 {
		t.Error("unauthenticated request changed the keepalive interval")
	}
}

// The panel reads these back through /api/status, so they have to be in there.
func TestStatusExposesSettings(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	s := newTestServer(upstreamSrv.URL, desktopCred(), nil)
	s.keepMinutes = 45

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Host = "localhost:7864"
	req.RemoteAddr = "127.0.0.1:54321" // handleStatus rejects anything but loopback
	s.handleStatus(rec, req)
	var payload struct {
		Settings  *settings `json:"settings"`
		Recovered int64     `json:"recovered"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Settings == nil || payload.Settings.KeepaliveMinutes != 45 {
		t.Errorf("settings in status = %+v", payload.Settings)
	}
}
