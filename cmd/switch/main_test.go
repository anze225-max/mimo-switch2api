package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExistingInstanceRecognisesOurOwnHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","plan":"desktop-free","upstream":"https://example.invalid/route"}`))
	}))
	defer srv.Close()

	if got := existingInstance(srv.Listener.Addr().String()); got == "" {
		t.Errorf("existingInstance = %q, want the address of the running proxy", got)
	}
}

// Someone else's server on the port must not silence us: the bind error is the diagnosis the
// user needs, whereas a quiet exit looks like the app refusing to start.
func TestExistingInstanceIgnoresForeignServers(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer foreign.Close()
	if got := existingInstance(foreign.Listener.Addr().String()); got != "" {
		t.Errorf("existingInstance = %q for a server without an upstream, want \"\"", got)
	}
}
