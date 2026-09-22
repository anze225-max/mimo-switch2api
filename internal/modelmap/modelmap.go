// Package modelmap resolves the model name a client sends into a real MiMo model id.
//
// Clients such as Codex show their own model labels ("5.6 Sol") and send their own ids,
// so matching is done on a normalised form rather than an exact string, and every unseen
// name is recorded so the mapping can be made explicit later.
package modelmap

import (
	"sort"
	"strings"
)

// Resolver maps incoming model names onto catalogue ids.
type Resolver struct {
	rules        []rule
	defaultModel string
	catalogue    map[string]bool
}

type rule struct {
	needle string
	target string
}

// New builds a resolver. catalogue is the set of real model ids the desktop offers;
// aliases maps a normalised keyword to a catalogue id.
func New(catalogue []string, aliases map[string]string, defaultModel string) *Resolver {
	r := &Resolver{
		catalogue:    map[string]bool{},
		defaultModel: defaultModel,
	}
	for _, id := range catalogue {
		if id != "" {
			r.catalogue[Normalise(id)] = true
		}
	}
	for keyword, target := range aliases {
		if keyword == "" || target == "" {
			continue
		}
		r.rules = append(r.rules, rule{needle: Normalise(keyword), target: target})
	}
	// Longest keyword first, so a specific rule beats a loose one.
	sort.Slice(r.rules, func(i, j int) bool {
		return len(r.rules[i].needle) > len(r.rules[j].needle)
	})
	return r
}

// Normalise lowercases and drops everything that is not a letter, digit or dot, so
// "5.6 Sol", "gpt-5.6-sol" and "5.6_Sol" all compare equal.
func Normalise(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Resolve returns the catalogue id to use and whether the incoming name was recognised
// (a known model, or matched by an alias rule).
func (r *Resolver) Resolve(incoming string) (target string, recognised bool) {
	norm := Normalise(incoming)
	if norm == "" {
		return r.defaultModel, false
	}
	if r.catalogue[norm] {
		return incoming, true
	}
	for _, rule := range r.rules {
		if strings.Contains(norm, rule.needle) {
			return rule.target, true
		}
	}
	return r.defaultModel, false
}

// DefaultAliases is the mapping the user asked for: the client's "5.6 Sol" tier runs on
// the cheap model and "6 Astra" on Pro. Keywords are matched rather than full ids, so the
// mapping survives the client renaming its labels. Anything else falls back to the default
// model and is recorded as an unknown name for later configuration.
func DefaultAliases() map[string]string {
	return map[string]string{
		"sol":   "mimo-v2.6-flash",
		"astra": "mimo-v2.6-pro",
	}
}
