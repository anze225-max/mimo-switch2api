package usage

import "testing"

func v26Catalog() []CatalogModel {
	return []CatalogModel{
		{ID: "mimo-v2.6-flash", Type: "TEXT", Ratio: 0.4},
		{ID: "mimo-v2.6-pro", Type: "TEXT", Ratio: 1.0},
		{ID: "mimo-v2.6-pro-ultraspeed", Type: "TEXT", Ratio: 10.0},
	}
}

func TestRecordWeightsByModelMultiplier(t *testing.T) {
	tr := NewTracker(v26Catalog())
	// 1000 tokens of Flash costs 0.4 units; the same of Pro costs 1.0.
	tr.Record("mimo-v2.6-flash", 600, 400, false)
	tr.Record("mimo-v2.6-pro", 600, 400, false)

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
	if flash, ok := seen["mimo-v2.6-flash"]; !ok || flash.Units != 0.4 {
		t.Errorf("flash accounting = %+v", seen["mimo-v2.6-flash"])
	}
}

func TestUltraspeedIsChargedTenfold(t *testing.T) {
	tr := NewTracker(v26Catalog())
	tr.Record("mimo-v2.6-pro-ultraspeed", 500, 500, false)
	if got := tr.Snapshot().WeightedUnits; got != 10.0 {
		t.Errorf("ultraspeed units = %v, want 10.0 — this model must never be the default", got)
	}
}

func TestUnknownModelIsNotUnderReported(t *testing.T) {
	tr := NewTracker(v26Catalog())
	tr.Record("some-client-model-name", 1000, 0, false)
	if got := tr.Snapshot().WeightedUnits; got != 10.0 {
		t.Errorf("unknown model = %v units, want the conservative maximum 10.0", got)
	}
}

func TestCatalogOverridesFallback(t *testing.T) {
	// A future MiMo ratio change is picked up from the catalogue without a rebuild.
	tr := NewTracker([]CatalogModel{{ID: "mimo-v2.6-pro", Ratio: 3.5}})
	tr.Record("mimo-v2.6-pro", 1000, 0, false)
	if got := tr.Snapshot().WeightedUnits; got != 3.5 {
		t.Errorf("catalogue ratio did not override the fallback: %v", got)
	}
}

func TestFailedCallsCountButSpendNothing(t *testing.T) {
	tr := NewTracker(v26Catalog())
	tr.Record("mimo-v2.6-pro", 0, 0, true)
	snap := tr.Snapshot()
	if snap.Requests != 1 || snap.Failed != 1 {
		t.Errorf("snap = %+v", snap)
	}
	if snap.WeightedUnits != 0 {
		t.Errorf("failed call charged %v units", snap.WeightedUnits)
	}
}

func TestPreferredModelAvoidsTheMostExpensive(t *testing.T) {
	models := v26Catalog()
	// flash(0.4), pro(1.0), ultraspeed(10.0) sorted by ratio: pro is the middle, and the
	// 10x model must never become the silent default.
	if got := PreferredModel(models); got != "mimo-v2.6-pro" {
		t.Errorf("PreferredModel = %q, want mimo-v2.6-pro", got)
	}
	if got := PreferredModel(models[:1]); got != "mimo-v2.6-flash" {
		t.Errorf("single-model catalogue = %q", got)
	}
	if got := PreferredModel(nil); got != "" {
		t.Errorf("empty catalogue = %q, want empty", got)
	}
}
