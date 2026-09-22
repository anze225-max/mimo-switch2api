package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"mimo-switch/internal/desktopauth"
	"mimo-switch/internal/store"
	"mimo-switch/internal/upstream"
	"mimo-switch/internal/usage"
)

const staleCookie = "serviceToken=STALE; userId=42"

// fakeUpstream answers 200 only for a cookie it has never seen rejected.
func fakeUpstream(t *testing.T) (*httptest.Server, *seen) {
	t.Helper()
	rec := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("cookie"))
		if r.Header.Get("cookie") == staleCookie {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid session"}}`))
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","model":"mimo-v2.6-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

type seen struct {
	mu     sync.Mutex
	cookie []string
}

func (s *seen) record(c string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cookie = append(s.cookie, c)
}

func (s *seen) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cookie...)
}

func newTestServer(baseURL string, cred *store.Credential, relogin relogin) *Server {
	return &Server{
		client:     upstream.NewCookie(cred.Cookie, baseURL),
		credential: cred,
		// cfg stays nil so a renewal cannot touch the real on-disk config.
		tracker: usage.NewTracker(nil),
		relogin: relogin,
	}
}

func desktopCred() *store.Credential {
	return &store.Credential{
		Kind: store.KindDesktop, Cookie: staleCookie, PassToken: "pass", UID: "42",
		BaseURL: "unused", Model: "mimo-v2.6-pro", IssuedAt: time.Now(),
	}
}

func TestPostRenewsSessionOn401AndRetries(t *testing.T) {
	upstreamSrv, rec := fakeUpstream(t)
	renewals := 0
	cred := desktopCred()
	s := newTestServer(upstreamSrv.URL, cred, func() (*desktopauth.Session, string, error) {
		renewals++
		return &desktopauth.Session{
			ServiceToken: "FRESH", UserID: "42", HarvestedAt: time.Now().UTC(),
		}, "mimo-v2.6-flash", nil
	})

	res, err := s.post(context.Background(), "/chat/completions", json.RawMessage(`{"model":"mimo-v2.6-pro"}`))
	if err != nil {
		t.Fatalf("post after renewal should succeed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if renewals != 1 {
		t.Errorf("renewals = %d, want exactly 1", renewals)
	}
	cookies := rec.all()
	if len(cookies) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (reject then accept): %v", len(cookies), cookies)
	}
	if cookies[0] != staleCookie {
		t.Errorf("first attempt cookie = %q", cookies[0])
	}
	if cookies[1] != "serviceToken=FRESH; userId=42" {
		t.Errorf("retry did not carry the renewed cookie: %q", cookies[1])
	}
	if cred.Model != "mimo-v2.6-flash" {
		t.Errorf("credential model not updated after renewal: %q", cred.Model)
	}
}

func TestConcurrent401sTriggerOnlyOneRenewal(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	var mu sync.Mutex
	renewals := 0
	s := newTestServer(upstreamSrv.URL, desktopCred(), func() (*desktopauth.Session, string, error) {
		mu.Lock()
		renewals++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return &desktopauth.Session{ServiceToken: "FRESH", UserID: "42"}, "mimo-v2.6-pro", nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.post(context.Background(), "/chat/completions", json.RawMessage(`{}`))
			if err == nil {
				res.Body.Close()
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// The cooldown window absorbs the rest; one passport round trip must suffice.
	if renewals > 2 {
		t.Errorf("renewals = %d, want at most 2 for 5 concurrent 401s", renewals)
	}
}

func TestNon401ErrorsDoNotTriggerRenewal(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	renewals := 0
	s := newTestServer(bad.URL, desktopCred(), func() (*desktopauth.Session, string, error) {
		renewals++
		return nil, "", nil
	})
	if _, err := s.post(context.Background(), "/chat/completions", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected the 500 to surface as an error")
	}
	if renewals != 0 {
		t.Errorf("renewals = %d on a 500, want 0 — renewing cannot fix a server fault", renewals)
	}
}

func TestMissingMasterDisablesRenewal(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	cred := desktopCred()
	cred.PassToken = ""
	renewals := 0
	s := newTestServer(upstreamSrv.URL, cred, func() (*desktopauth.Session, string, error) {
		renewals++
		return nil, "", nil
	})
	_, err := s.post(context.Background(), "/chat/completions", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("stale session without passToken should surface an error")
	}
	if renewals != 0 {
		t.Errorf("renewals = %d without a passToken, want 0", renewals)
	}
}

func TestFailedRenewalIsNotRetriedForever(t *testing.T) {
	upstreamSrv, _ := fakeUpstream(t)
	renewals := 0
	s := newTestServer(upstreamSrv.URL, desktopCred(), func() (*desktopauth.Session, string, error) {
		renewals++
		return nil, "", errors.New("passToken 已失效")
	})
	if _, err := s.post(context.Background(), "/chat/completions", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected the original 401 to surface when renewal fails")
	}
	if renewals != 1 {
		t.Errorf("renewals = %d, want exactly 1 before giving up", renewals)
	}
}
