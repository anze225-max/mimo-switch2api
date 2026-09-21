// Package usage tallies what the proxy has consumed, in the desktop quota's own units.
package usage

import (
	"sync"
	"time"
)

// Multipliers are what MiMo's desktop model picker charges per token: Flash costs 0.4 of
// the pool where Pro costs 1.0. Quota is spent in these weighted units, not raw tokens.
var Multipliers = map[string]float64{
	"mimo-x-flash-preview": 0.4,
	"mimo-x-pro-preview":   1.0,
}

// DefaultMultiplier applies to models we do not know the rate of, so the panel never
// under-reports.
const DefaultMultiplier = 1.0

type Model struct {
	ID         string  `json:"id"`
	Multiplier float64 `json:"multiplier"`
}

// Models returns the catalogue for the panel, Pro first as the desktop does.
func Models() []Model {
	return []Model{
		{ID: "mimo-x-pro-preview", Multiplier: 1.0},
		{ID: "mimo-x-flash-preview", Multiplier: 0.4},
	}
}

func MultiplierFor(model string) float64 {
	if m, ok := Multipliers[model]; ok {
		return m
	}
	return DefaultMultiplier
}

type Snapshot struct {
	Requests        int64     `json:"requests"`
	Failed          int64     `json:"failed"`
	PromptTokens    int64     `json:"prompt_tokens"`
	CompletionToken int64     `json:"completion_tokens"`
	WeightedUnits   float64   `json:"weighted_units"`
	ByModel         []ByModel `json:"by_model"`
	Since           time.Time `json:"since"`
}

type ByModel struct {
	Model      string  `json:"model"`
	Requests   int64   `json:"requests"`
	Units      float64 `json:"units"`
	Multiplier float64 `json:"multiplier"`
}

// Tracker is the single accounting point for the proxy.
type Tracker struct {
	mu       sync.Mutex
	since    time.Time
	requests int64
	failed   int64
	in       int64
	out      int64
	weighted float64
	perModel map[string]*ByModel
}

func NewTracker() *Tracker {
	return &Tracker{since: time.Now(), perModel: map[string]*ByModel{}}
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
	units := float64(promptTokens+completionTokens) * MultiplierFor(model) / 1000.0
	t.weighted += units

	entry, ok := t.perModel[model]
	if !ok {
		entry = &ByModel{Model: model, Multiplier: MultiplierFor(model)}
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
	}
}
