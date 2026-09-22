package login

import (
	"encoding/json"
	"testing"
)

type cookie [3]string // name, value, domain

func cookiesJSON(t *testing.T, cs ...cookie) json.RawMessage {
	t.Helper()
	list := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		domain := c[2]
		if domain == "" {
			domain = ".xiaomi.com"
		}
		list = append(list, map[string]any{"name": c[0], "value": c[1], "domain": domain, "path": "/"})
	}
	raw, err := json.Marshal(map[string]any{"cookies": list})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPicksPassportSessionCookies(t *testing.T) {
	win, err := parseWindowCookies(cookiesJSON(t,
		cookie{"userId", " 12345 ", ".account.xiaomi.com"},
		cookie{"passToken", "pt-secret", ".account.xiaomi.com"},
		cookie{"cUserId", "cu-secret", ".account.xiaomi.com"},
		cookie{"deviceId", "irrelevant", ""},
	))
	if err != nil {
		t.Fatal(err)
	}
	m := win.Master
	if m.PassToken != "pt-secret" || m.UserID != "12345" || m.CUserId != "cu-secret" {
		t.Fatalf("master = %+v", m)
	}
	if m.SID != "mimopc" {
		t.Errorf("SID = %q, want the desktop service id so Refresh mints a usable token", m.SID)
	}
}

func TestFirstNonBlankIdentityCookieWins(t *testing.T) {
	win, err := parseWindowCookies(cookiesJSON(t,
		cookie{"userId", "111", ".account.xiaomi.com"},
		cookie{"userId", "222", ".xiaomimimo.com"},
		cookie{"passToken", "", ".account.xiaomi.com"},
		cookie{"passToken", "kept", ".account.xiaomi.com"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if win.Master.UserID != "111" {
		t.Errorf("UserID = %q, want the first value", win.Master.UserID)
	}
	if win.Master.PassToken != "kept" {
		t.Errorf("PassToken = %q, want the blank one skipped", win.Master.PassToken)
	}
}

// MiMo keeps its serviceToken on its own host: once the login redirect got that far the
// token is already usable and the passport exchange is unnecessary.
func TestMimoScopedServiceTokenIsTakenDirectly(t *testing.T) {
	win, err := parseWindowCookies(cookiesJSON(t,
		cookie{"serviceToken", "direct", ".mimo-server-cn.xiaomimimo.com"},
		cookie{"userId", "42", ".xiaomimimo.com"},
		cookie{"passToken", "pt", ".account.xiaomi.com"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if win.ServiceToken != "direct" {
		t.Fatalf("ServiceToken = %q, want the one scoped to MiMo's host", win.ServiceToken)
	}
}

// A serviceToken for some other service (passport itself) is not MiMo's ticket.
func TestOtherServicesServiceTokenIsIgnored(t *testing.T) {
	win, err := parseWindowCookies(cookiesJSON(t,
		cookie{"serviceToken", "for-passport", ".passport.api.xiaomi.com"},
		cookie{"passToken", "pt", ".account.xiaomi.com"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if win.ServiceToken != "" {
		t.Errorf("ServiceToken = %q, want it ignored so Refresh is used instead", win.ServiceToken)
	}
}

func TestUnsignedInWindowReadsAsEmpty(t *testing.T) {
	win, err := parseWindowCookies(cookiesJSON(t, cookie{"uLocale", "zh_CN", ""}))
	if err != nil {
		t.Fatal(err)
	}
	if win.ServiceToken != "" || win.Master.PassToken != "" || win.Master.UserID != "" {
		t.Fatalf("win = %+v, want an empty read so the caller keeps waiting", win)
	}
}

func TestMalformedCookieReplyIsAnError(t *testing.T) {
	if _, err := parseWindowCookies(json.RawMessage(`{"cookies":"no"}`)); err == nil {
		t.Fatal("want an error for a reply we cannot parse")
	}
}

// Passport answers 70016 unless Phase 1 sees the whole jar the sign-in window ended with,
// not just the master trio, so every cookie name has to ride along.
func TestSignInWindowJarIsCarriedWhole(t *testing.T) {
	win, err := parseWindowCookies(cookiesJSON(t,
		cookie{"passToken", "pt", ".account.xiaomi.com"},
		cookie{"userId", "42", ".account.xiaomi.com"},
		cookie{"sgn", "abc", ".xiaomi.com"},
		cookie{"bav", "def", ".xiaomi.com"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if want := "bav=def; passToken=pt; sgn=abc; userId=42"; win.Master.SessionCookies != want {
		t.Errorf("SessionCookies = %q, want %q", win.Master.SessionCookies, want)
	}
}

// An empty jar must leave the field blank so Refresh keeps its three-cookie renewal path,
// which is what the silent-renewal case relies on.
func TestEmptyWindowJarFallsBackToTheMasterTrio(t *testing.T) {
	win, err := parseWindowCookies(cookiesJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	if win.Master.SessionCookies != "" {
		t.Errorf("SessionCookies = %q, want empty", win.Master.SessionCookies)
	}
}
