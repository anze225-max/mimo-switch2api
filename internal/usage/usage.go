// Package usage tallies what the proxy has consumed, in the desktop quota's own units.
package usage

import (
	"sort"
	"sync"
	"time"
)

// FallbackMultipliers are only used when MiMo's catalogue cannot be read. They match the
// v2.6 generation; older entries are kept so historical usage still accounts sensibly.
var FallbackMultipliers = map[string]float64{
	"mimo-v2.6-flash":          0.4,
	"mimo-v2.6-pro":            1.0,
	"mimo-v2.6-pro-ultraspeed": 10.0,
	"mimo-x-flash-preview":     0.4,
	"mimo-x-pro-preview":       1.0,
}

// DefaultMultiplier applies to models we do not know the rate of, so the panel never
// under-reports.
const DefaultMultiplier = 10.0

type Model struct {
	ID         string  `json:"id"`
	Multiplier float64 `json:"multiplier"`
}

// Tracker is the single accounting point for the proxy. Its multipliers come from the live
// catalogue so a MiMo model rename cannot silently mis-price the quota display.
type Tracker struct {
	mu           sync.Mutex
	since        time.Time
	multipliers  map[string]float64
	requests     int64
	failed       int64
	in           int64
	out          int64
	weighted     float64
	perModel     map[string]*ByModel
	clientModels map[string]int
	unmapped     map[string]int
}

func NewTracker(models []CatalogModel) *Tracker {
	multipliers := map[string]float64{}
	for id, ratio := range FallbackMultipliers {
		multipliers[id] = ratio
	}
	for _, m := range models {
		if m.Ratio > 0 {
			multipliers[m.ID] = m.Ratio
		}
	}
	return &Tracker{
		since: time.Now(), multipliers: multipliers, perModel: map[string]*ByModel{},
		clientModels: map[string]int{}, unmapped: map[string]int{},
	}
}

// RecordModelName notes what a client actually asked for. mapped=false means no alias or
// catalogue entry matched, so the request silently used the default model.
func (t *Tracker) RecordModelName(sent string, mapped bool) {
	if sent == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clientModels[sent]++
	if !mapped {
		t.unmapped[sent]++
	}
}

// multiplierFor must be called with t.mu held; Record and SetMultipliers own the locking.
func (t *Tracker) multiplierFor(model string) float64 {
	if r, ok := t.multipliers[model]; ok {
		return r
	}
	return DefaultMultiplier
}

// SetMultipliers adopts a freshly read catalogue, so a MiMo update that changes ratios is
// reflected without a restart.
func (t *Tracker) SetMultipliers(models []CatalogModel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range models {
		if m.Ratio > 0 {
			t.multipliers[m.ID] = m.Ratio
		}
	}
}

type Snapshot struct {
	Requests        int64     `json:"requests"`
	Failed          int64     `json:"failed"`
	PromptTokens    int64     `json:"prompt_tokens"`
	CompletionToken int64     `json:"completion_tokens"`
	WeightedUnits   float64   `json:"weighted_units"`
	ByModel         []ByModel `json:"by_model"`
	// ClientModels lists the raw model names clients actually sent, so an alias can be
	// configured against evidence instead of guessing what a picker label maps to on the wire.
	ClientModels []string  `json:"client_models"`
	Unmapped     []string  `json:"unmapped_models"`
	Since        time.Time `json:"since"`
}

type ByModel struct {
	Model      string  `json:"model"`
	Requests   int64   `json:"requests"`
	Units      float64 `json:"units"`
	Multiplier float64 `json:"multiplier"`
}

// Record adds one completed upstream call. Tokens come from the upstream's own usage
// report, never an estimate, so the panel cannot drift from what MiMo charges.
func (t *Tracker) Record(model string, promptTokens, completionTokens int, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requests++
	if failed {
		t.failed++
		return
	}
	t.in += int64(promptTokens)
	t.out += int64(completionTokens)
	ratio := t.multiplierFor(model)
	units := float64(promptTokens+completionTokens) * ratio / 1000.0
	t.weighted += units

	entry, ok := t.perModel[model]
	if !ok {
		entry = &ByModel{Model: model, Multiplier: ratio}
		t.perModel[model] = entry
	}
	entry.Requests++
	entry.Units += units
}

func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	var models []ByModel
	for _, e := range t.perModel {
		models = append(models, *e)
	}
	return Snapshot{
		Requests: t.requests, Failed: t.failed,
		PromptTokens: t.in, CompletionToken: t.out,
		WeightedUnits: t.weighted, ByModel: models, Since: t.since,
		ClientModels: keysByCount(t.clientModels),
		Unmapped:     keysByCount(t.unmapped),
	}
}

// keysByCount returns names ordered by descending hit count, so the panel shows the model
// labels a client uses most often first.
func keysByCount(counts map[string]int) []string {
	out := make([]string, 0, len(counts))
	for k := range counts {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
