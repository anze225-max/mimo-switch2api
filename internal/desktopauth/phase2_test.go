package desktopauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// MiMo's own SSO manager states it outright — "Phase 2 request should NOT include Cookie
// header" — and sending the session there makes passport answer 401. Phase 1 is the opposite:
// it carries exactly the master trio and nothing else.
func TestPhase1CarriesTheTrioAndPhase2CarriesNothing(t *testing.T) {
	original := passportStep1
	t.Cleanup(func() { passportStep1 = original })

	var phase1Cookie, phase2Cookie, phase2UA string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/pass/serviceLogin"):
			phase1Cookie = r.Header.Get("Cookie")
			w.Header().Add("Set-Cookie", "bav=from-phase-one; Path=/")
			fmt.Fprintf(w, "%s{\"code\":0,\"location\":\"%s/loc?redirect=1\",\"ssecurity\":\"SEC\",\"nonce\":\"123456\"}",
				jsonHijackPfx, srv.URL)
		case r.URL.Path == "/loc":
			phase2Cookie, phase2UA = r.Header.Get("Cookie"), r.Header.Get("User-Agent")
			http.SetCookie(w, &http.Cookie{Name: "serviceToken", Value: "FINAL", Path: "/"})
			w.WriteHeader(http.StatusOK)
		default:
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
	for _, want := range []string{"userId=42", "passToken=pt", "cUserId=cu"} {
		if !strings.Contains(phase1Cookie, want) {
			t.Errorf("Phase 1 sent Cookie %q, missing %s", phase1Cookie, want)
		}
	}
	if phase2Cookie != "" {
		t.Errorf("Phase 2 sent Cookie %q, must travel without any", phase2Cookie)
	}
	if phase2UA != passportUA {
		t.Errorf("Phase 2 User-Agent = %q, want %q", phase2UA, passportUA)
	}
}
