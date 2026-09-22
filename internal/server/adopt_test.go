package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"mimo-switch/internal/desktopauth"
	"mimo-switch/internal/modelmap"
	"mimo-switch/internal/store"
)

// The distributed app lets the user sign in while the proxy is already running, so adopting
// that session has to take effect on the next request without a restart.
func TestAdoptDesktopInstallsSessionLive(t *testing.T) {
	upstreamSrv, rec := fakeUpstream(t)
	s := newTestServer(upstreamSrv.URL, &store.Credential{
		Kind: store.KindPlatform, SK: "sk-old", BaseURL: upstreamSrv.URL, Model: "gone-model",
	}, nil)
	s.verify = func(*desktopauth.Session) (string, error) { return "mimo-v2.6-flash", nil }

	err := s.AdoptDesktop(&desktopauth.Session{
		ServiceToken: "NEW", UserID: "42", PassToken: "master", CUserId: "cu",
		HarvestedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AdoptDesktop: %v", err)
	}

	cred := s.credential()
	if cred.Kind != store.KindDesktop || cred.UID != "42" || cred.PassToken != "master" || cred.CUserId != "cu" {
		t.Fatalf("live credential = %+v", cred)
	}
	if cred.BaseURL != s.desktopEndpoint() {
		t.Errorf("BaseURL = %q, want the desktop endpoint the session was adopted into", cred.BaseURL)
	}
	if client := s.client; client.BaseURL() != upstreamSrv.URL {
		t.Errorf("client base URL = %q, want the test upstream", client.BaseURL())
	}

	res, err := s.post(context.Background(), "/chat/completions", json.RawMessage(`{"model":"whatever"}`))
	if err != nil {
		t.Fatalf("post after adoption: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	cookies := rec.all()
	if len(cookies) != 1 || cookies[0] != "serviceToken=NEW; userId=42" {
		t.Errorf("upstream saw %v, want the adopted session cookie", cookies)
	}
}

// After adoption the fallback model is the new one, so a client name we do not recognise
// must not keep pointing at the model from the previous session.
func TestAdoptDesktopRebuildsTheAliasFallback(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	s := newTestServer(upstreamSrv.URL, &store.Credential{
		Kind: store.KindPlatform, SK: "sk-old", BaseURL: upstreamSrv.URL, Model: "gone-model",
	}, nil)
	s.aliases = modelmap.DefaultAliases()
	s.verify = func(*desktopauth.Session) (string, error) { return "mimo-v2.6-pro", nil }

	if err := s.AdoptDesktop(&desktopauth.Session{ServiceToken: "NEW", UserID: "42", HarvestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if got, recognised := s.resolver().Resolve("完全没听过的名字"); got != "mimo-v2.6-pro" || recognised {
		t.Errorf("unmapped name resolved to %q (recognised=%v), want the new default", got, recognised)
	}
	if got, _ := s.resolver().Resolve("5.6 Sol"); got != "mimo-v2.6-flash" {
		t.Errorf("alias resolved to %q, want the configured flash", got)
	}
}
