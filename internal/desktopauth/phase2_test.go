package desktopauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Phase 2 must still carry the master trio even when Phase 1 answered with cookies of its
// own: Go appends the jar's share to the header, so both survive. A negative control (an
// explicit header only) would pass too, so the assertion is about the pair being present.
func TestPhase2CarriesTheMasterCookiesAlongsidePhase1s(t *testing.T) {
	original := passportStep1
	t.Cleanup(func() { passportStep1 = original })

	var phase2Cookie string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/pass/serviceLogin"):
			w.Header().Add("Set-Cookie", "bav=from-phase-one; Path=/")
			fmt.Fprintf(w, "%s{\"code\":0,\"location\":\"%s/loc?redirect=1\",\"ssecurity\":\"SEC\",\"nonce\":\"123456\"}",
				jsonHijackPfx, srv.URL)
		case r.URL.Path == "/loc": // passport's loc already carries a query
			phase2Cookie = r.Header.Get("Cookie")
			http.SetCookie(w, &http.Cookie{Name: "serviceToken", Value: "FINAL", Path: "/"})
			w.WriteHeader(http.StatusOK)
		default:
			t.Logf("未预期的请求: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	passportStep1 = srv.URL + "/pass/serviceLogin"

	session, err := Refresh(MasterCredential{
		PassToken: "pt", UserID: "42", CUserId: "cu", SID: DefaultSID,
		SessionCookies: "bav=stale; passToken=pt; userId=42; cUserId=cu",
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if session.ServiceToken != "FINAL" {
		t.Errorf("serviceToken = %q, want FINAL", session.ServiceToken)
	}
	for _, want := range []string{"passToken=pt", "userId=42", "cUserId=cu"} {
		if !strings.Contains(phase2Cookie, want) {
			t.Errorf("Phase 2 sent Cookie %q, missing %s", phase2Cookie, want)
		}
	}
}

// Without a jar seed the renewal case must still send exactly the master trio, which is what
// MiMo itself sends (its code notes ssecurity/nonce are not sent in Phase 1).
func TestRenewalSendsOnlyTheMasterTrio(t *testing.T) {
	original := passportStep1
	t.Cleanup(func() { passportStep1 = original })

	var phase1Cookie string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/pass/serviceLogin") {
			phase1Cookie = r.Header.Get("Cookie")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	passportStep1 = srv.URL + "/pass/serviceLogin"

	if _, err := Refresh(MasterCredential{PassToken: "pt", UserID: "42", SID: DefaultSID}); err == nil {
		t.Fatal("want a failure once Phase 1 stops short of returning a location")
	}
	if phase1Cookie != "userId=42; passToken=pt" {
		t.Errorf("Phase 1 sent Cookie %q, want only the master trio", phase1Cookie)
	}
}
