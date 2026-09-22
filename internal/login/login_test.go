package login

import (
	"encoding/json"
	"testing"
)

func cookiesJSON(t *testing.T, pairs ...[2]string) json.RawMessage {
	t.Helper()
	type cookie struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Domain string `json:"domain"`
		Path   string `json:"path"`
	}
	cookies := make([]cookie, 0, len(pairs))
	for _, p := range pairs {
		cookies = append(cookies, cookie{Name: p[0], Value: p[1], Domain: ".xiaomi.com", Path: "/"})
	}
	raw, err := json.Marshal(map[string]any{"cookies": cookies})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPicksPassportSessionCookies(t *testing.T) {
	master, err := parsePassportCookies(cookiesJSON(t,
		[2]string{"userId", " 12345 "},
		[2]string{"passToken", "pt-secret"},
		[2]string{"cUserId", "cu-secret"},
		[2]string{"deviceId", "irrelevant"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if master.PassToken != "pt-secret" || master.UserID != "12345" || master.CUserId != "cu-secret" {
		t.Fatalf("master = %+v", master)
	}
	if master.SID != "mimopc" {
		t.Errorf("SID = %q, want the desktop service id so Refresh mints a usable token", master.SID)
	}
}

func TestFirstNonBlankIdentityCookieWins(t *testing.T) {
	master, err := parsePassportCookies(cookiesJSON(t,
		[2]string{"userId", "111"},
		[2]string{"userId", "222"},
		[2]string{"passToken", ""},
		[2]string{"passToken", "kept"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if master.UserID != "111" {
		t.Errorf("UserID = %q, want the first value", master.UserID)
	}
	if master.PassToken != "kept" {
		t.Errorf("PassToken = %q, want the blank one skipped", master.PassToken)
	}
}

func TestUnsignedInWindowReadsAsEmpty(t *testing.T) {
	master, err := parsePassportCookies(cookiesJSON(t, [2]string{"uLocale", "zh_CN"}))
	if err != nil {
		t.Fatal(err)
	}
	if master.PassToken != "" || master.UserID != "" {
		t.Fatalf("master = %+v, want an empty master so the caller keeps waiting", master)
	}
}

func TestMalformedCookieReplyIsAnError(t *testing.T) {
	if _, err := parsePassportCookies(json.RawMessage(`{"cookies":"no"}`)); err == nil {
		t.Fatal("want an error for a reply we cannot parse")
	}
}

// Passport answers 70016 unless Phase 1 sees the whole jar the sign-in window ended with,
// not just the master trio, so every cookie name has to ride along.
func TestSignInWindowJarIsCarriedWhole(t *testing.T) {
	master, err := parsePassportCookies(cookiesJSON(t,
		[2]string{"passToken", "pt"},
		[2]string{"userId", "42"},
		[2]string{"sgn", "abc"},
		[2]string{"bav", "def"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if want := "bav=def; passToken=pt; sgn=abc; userId=42"; master.SessionCookies != want {
		t.Errorf("SessionCookies = %q, want %q", master.SessionCookies, want)
	}
}

// An empty jar must leave the field blank so Refresh keeps its three-cookie renewal path
// (that is the silent-renewal case, where no window was ever involved).
func TestEmptyWindowJarFallsBackToTheMasterTrio(t *testing.T) {
	master, err := parsePassportCookies(cookiesJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	if master.SessionCookies != "" {
		t.Errorf("SessionCookies = %q, want empty", master.SessionCookies)
	}
}
