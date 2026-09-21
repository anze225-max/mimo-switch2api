package usage

import "testing"

func TestRecordWeightsByModelMultiplier(t *testing.T) {
	tr := NewTracker()
	// 1000 tokens of Flash costs 0.4 units; the same of Pro costs 1.0.
	tr.Record("mimo-x-flash-preview", 600, 400, false)
	tr.Record("mimo-x-pro-preview", 600, 400, false)

	snap := tr.Snapshot()
	if snap.Requests != 2 || snap.Failed != 0 {
		t.Fatalf("requests = %d failed = %d", snap.Requests, snap.Failed)
	}
	if snap.PromptTokens != 1200 || snap.CompletionToken != 800 {
		t.Errorf("tokens = %d/%d, want 1200/800", snap.PromptTokens, snap.CompletionToken)
	}
	if got, want := snap.WeightedUnits, 1.4; got != want {
		t.Errorf("weighted units = %v, want %v (0.4 + 1.0)", got, want)
	}
	seen := map[string]ByModel{}
	for _, m := range snap.ByModel {
		seen[m.Model] = m
	}
	if flash, ok := seen["mimo-x-flash-preview"]; !ok || flash.Units != 0.4 {
		t.Errorf("flash accounting = %+v", seen["mimo-x-flash-preview"])
	}
}

func TestUnknownModelIsNotUnderReported(t *testing.T) {
	tr := NewTracker()
	tr.Record("some-client-model-name", 1000, 0, false)
	if got := tr.Snapshot().WeightedUnits; got != 1.0 {
		t.Errorf("unknown model multiplier = %v, want the conservative 1.0", got)
	}
}

func TestFailedCallsCountButSpendNothing(t *testing.T) {
	tr := NewTracker()
	tr.Record("mimo-x-pro-preview", 0, 0, true)
	snap := tr.Snapshot()
	if snap.Requests != 1 || snap.Failed != 1 {
		t.Errorf("snap = %+v", snap)
	}
	if snap.WeightedUnits != 0 {
		t.Errorf("failed call charged %v units", snap.WeightedUnits)
	}
}

func TestMultiplierLookup(t *testing.T) {
	if got := MultiplierFor("mimo-x-flash-preview"); got != 0.4 {
		t.Errorf("flash multiplier = %v, want 0.4 (matches the desktop picker)", got)
	}
	if got := MultiplierFor("mimo-x-pro-preview"); got != 1.0 {
		t.Errorf("pro multiplier = %v, want 1.0", got)
	}
	if len(Models()) != 2 {
		t.Errorf("Models() = %d entries, want the two desktop text models", len(Models()))
	}
}
