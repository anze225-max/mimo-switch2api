package desktopauth

import "testing"

// Pinned vectors computed independently of this implementation: the signature formula is
// urlencode(base64(sha1("nonce=<nonce>&<ssecurity>"))), a bare SHA-1 over a concatenation
// rather than an HMAC. If someone "improves" this into HMAC or reorders the input, these
// fail, which is the point.
func TestClientSignPinnedVectors(t *testing.T) {
	cases := []struct {
		nonce, ssecurity, want string
	}{
		{"1234567890", "abcDEF123==", "MlAF8oukNi9pWgGu6ziRaxdKMk4%3D"},
		{"1234567890", "", "8xp0uVwCvmjOrU2YTr3sinJXBlw%3D"},
		{"1234567890", "   ", "8xp0uVwCvmjOrU2YTr3sinJXBlw%3D"},
	}
	for _, c := range cases {
		if got := ClientSign(c.nonce, c.ssecurity); got != c.want {
			t.Errorf("ClientSign(%q, %q) = %q, want %q", c.nonce, c.ssecurity, got, c.want)
		}
	}
}

func TestParsePhase1StripsHijackPrefixAndKeepsNonceText(t *testing.T) {
	body := jsonHijackPfx + `{"code":0,"location":"https://account.xiaomi.com/pass/serviceLogin?sid=mimopc&xx=1","ssecurity":"sec","nonce":1758451200123}`
	p, err := parsePhase1(body)
	if err != nil {
		t.Fatal(err)
	}
	if p.Code != 0 || p.Security != "sec" {
		t.Errorf("phase1 = %+v", p)
	}
	// A large nonce must survive as decimal text; float64 would corrupt the signature.
	if p.Nonce.String() != "1758451200123" {
		t.Errorf("nonce = %q, want the exact decimal text", p.Nonce.String())
	}
	if p.Location == "" {
		t.Error("location must map from the JSON location field")
	}
}

func TestParsePhase1ReportsDeadPassToken(t *testing.T) {
	p, err := parsePhase1(jsonHijackPfx + `{"code":401,"location":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Code != 401 {
		t.Errorf("code = %d", p.Code)
	}
}

func TestMasterCredentialCookieHeader(t *testing.T) {
	m := MasterCredential{UserID: "42", PassToken: "pt", CUserId: "cu"}
	if got := m.cookieHeader(); got != "userId=42; passToken=pt; cUserId=cu" {
		t.Errorf("header = %q", got)
	}
	optional := MasterCredential{UserID: "42", PassToken: "pt"}
	if got := optional.cookieHeader(); got != "userId=42; passToken=pt" {
		t.Errorf("header without cUserId = %q", got)
	}
}

func TestRefreshRefusesWithoutMasterCredential(t *testing.T) {
	if _, err := Refresh(MasterCredential{}); err == nil {
		t.Fatal("refresh without passToken should be refused before any network call")
	}
}
