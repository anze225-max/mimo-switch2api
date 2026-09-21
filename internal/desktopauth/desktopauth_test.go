package desktopauth

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseExportPicksTheMimoServerSession(t *testing.T) {
	raw, err := json.Marshal([]chromiumCookie{
		{Name: "userId", Value: "account-uid", Domain: ".account.xiaomi.com"},
		{Name: "userId", Value: "3207174710", Domain: ".xiaomimimo.com"},
		{Name: "serviceToken", Value: strings.Repeat("s", 360), Domain: ".mimo-server-cn.xiaomimimo.com"},
		{Name: "passToken", Value: strings.Repeat("p", 340), Domain: ".account.xiaomi.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseExport(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.UserID != "3207174710" {
		t.Errorf("userId should come from the xiaomimimo domain, got %q", s.UserID)
	}
	if s.PassToken == "" {
		t.Error("passToken not captured")
	}
	header := s.CookieHeader()
	if !strings.HasPrefix(header, "serviceToken=") {
		t.Errorf("cookie header = %q", header)
	}
	if !strings.HasSuffix(header, "userId=3207174710") {
		t.Errorf("cookie header must carry userId for mimo-server, got %q", header)
	}
}

func TestParseExportRejectsMissingSession(t *testing.T) {
	if _, err := ParseExport([]byte(`[{"name":"uLocale","value":"zh_CN","domain":".xiaomi.com"}]`)); err == nil {
		t.Fatal("export without serviceToken accepted")
	}
}

func TestParseExportOmitsUserIDWhenAbsent(t *testing.T) {
	s, err := ParseExport([]byte(`[{"name":"serviceToken","value":"tok","domain":".mimo-server-cn.xiaomimimo.com"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CookieHeader(); got != "serviceToken=tok" {
		t.Errorf("cookie header = %q", got)
	}
}

func TestCookieSnippetIsEvaluableShape(t *testing.T) {
	// The snippet must be valid JS wrapped by cdp.Evaluate's async shell: it may not use
	// a top-level return, since the caller supplies the enclosing function.
	if strings.Contains(cookieSnippet, "\nreturn JSON") == false {
		t.Fatal("snippet no longer returns the jar; harvest would silently break")
	}
	if !strings.Contains(cookieSnippet, "persist:xiaomi-account") {
		t.Fatal("snippet must read the login partition, not the default session")
	}
}
