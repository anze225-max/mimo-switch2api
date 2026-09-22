package modelmap

import "testing"

func newTestResolver() *Resolver {
	return New(
		[]string{"mimo-v2.6-flash", "mimo-v2.6-pro", "mimo-v2.6-pro-ultraspeed"},
		DefaultAliases(),
		"mimo-v2.6-flash",
	)
}

func TestNormaliseIgnoresPunctuationAndCase(t *testing.T) {
	cases := map[string]string{
		"5.6 Sol":      "5.6sol",
		"5.6_Sol":      "5.6sol",
		"  5.6  SOL  ": "5.6sol",
		// Letters survive normalisation, so a vendor prefix stays in the string; alias
		// matching uses substring containment precisely so this still resolves.
		"gpt-5.6-sol": "gpt5.6sol",
	}
	for in, want := range cases {
		if got := Normalise(in); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

// A client that capitalises or spaces a real id still resolves, and the upstream is sent
// the exact lowercase id it will accept.
func TestKnownModelInAnyCaseResolvesToTheCanonicalID(t *testing.T) {
	r := newTestResolver()
	for _, in := range []string{"MiMo V2.6 Pro", "mimo-v2.6-pro", "MIMO-V2.6-PRO"} {
		target, recognised := r.Resolve(in)
		if !recognised {
			t.Errorf("Resolve(%q) says the client name is unknown", in)
			continue
		}
		if target != "mimo-v2.6-pro" {
			t.Errorf("Resolve(%q) = %q, want the canonical id", in, target)
		}
	}
}

func TestClientLabelsResolveToTheRequestedTiers(t *testing.T) {
	r := newTestResolver()
	cases := []struct {
		in, want string
	}{
		{"5.6 Sol", "mimo-v2.6-flash"},
		{"gpt-5.6-sol", "mimo-v2.6-flash"},
		{"5.6-SOL", "mimo-v2.6-flash"},
		{"6 Astra", "mimo-v2.6-pro"},
		{"astra", "mimo-v2.6-pro"},
	}
	for _, c := range cases {
		got, ok := r.Resolve(c.in)
		if got != c.want {
			t.Errorf("Resolve(%q) = %q, want %q", c.in, got, c.want)
		}
		if !ok {
			t.Errorf("Resolve(%q) should be recognised", c.in)
		}
	}
}

func TestRealModelIdsPassThrough(t *testing.T) {
	r := newTestResolver()
	for _, id := range []string{"mimo-v2.6-flash", "mimo-v2.6-pro", "mimo-v2.6-pro-ultraspeed"} {
		got, ok := r.Resolve(id)
		if got != id || !ok {
			t.Errorf("Resolve(%q) = %q ok=%v, want unchanged and recognised", id, got, ok)
		}
	}
}

func TestUnknownNamesFallBackButAreFlagged(t *testing.T) {
	r := newTestResolver()
	got, ok := r.Resolve("claude-sonnet-4-5-20250514")
	if got != "mimo-v2.6-flash" {
		t.Errorf("unknown should fall back to the default, got %q", got)
	}
	if ok {
		t.Error("an unknown name must be reported unrecognised so the panel can surface it")
	}
}

func TestLongerKeywordsWin(t *testing.T) {
	r := New([]string{"mimo-a", "mimo-b"},
		map[string]string{"sol": "mimo-b", "5.6sol": "mimo-a"}, "mimo-b")
	if got, _ := r.Resolve("5.6 Sol"); got != "mimo-a" {
		t.Errorf("specific rule should beat the loose one, got %q", got)
	}
}

func TestEmptyInputUsesDefault(t *testing.T) {
	r := newTestResolver()
	if got, ok := r.Resolve("   "); got != "mimo-v2.6-flash" || ok {
		t.Errorf("empty model = %q ok=%v", got, ok)
	}
}
