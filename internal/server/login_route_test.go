package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The login route opens a browser window, so it must not be reachable without the local
// token: a form POST is a CORS "simple request", meaning any web page this machine visits
// could otherwise fire it.
func TestLoginRouteRejectsCallersWithoutTheToken(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	s := newTestServer(upstreamSrv.URL, desktopCred(), nil)
	s.localToken = "secret-token"
	s.requireToken = true
	handler := s.Handler()

	cases := []struct {
		name  string
		value string
	}{
		{"no header at all", ""},
		{"wrong token", "Bearer nope"},
		{"empty bearer", "Bearer "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
			req.RemoteAddr = "127.0.0.1:54321"
			req.Host = "localhost:7864"
			if c.value != "" {
				req.Header.Set("authorization", c.value)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (and no window may open)", rec.Code)
			}
		})
	}
}
