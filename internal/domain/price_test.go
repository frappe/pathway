package domain

import (
	_ "embed"
	"encoding/json"
	"testing"
)

// shippedCounters is the control plane's table as the catalog ships it
// (grove/catalog/catalog.json); the metering and transport tests read the same file.
//
//go:embed testdata/counters.json
var shippedCounters []byte

func counters(t *testing.T) CounterTable {
	t.Helper()
	var table CounterTable
	if err := json.Unmarshal(shippedCounters, &table); err != nil {
		t.Fatal(err)
	}
	return table
}

// Truncation is per counter, so a divisor never rounds one counter's remainder into another's,
// and a counter the route does not price contributes nothing rather than failing the sum.
func TestCost(t *testing.T) {
	// $3/Mtok, $15/Mtok, $40/Mtok of audio in and $80/Mtok out, $0.000001/request, and one nano-USD
	// per Mtok written.
	rates := map[string]int64{
		"prompt_tokens": 3e9, "completion_tokens": 15e9, "audio_tokens": 40e9, "request_count": 1_000,
		"cache_write_tokens": 1, "cache_write_1h_tokens": 1, "completion_audio_tokens": 80e9,
	}
	cases := []struct {
		name   string
		counts map[string]int64
		want   int64
	}{
		{"prompt and completion", map[string]int64{"prompt_tokens": 1_000_000, "completion_tokens": 500_000}, 3_000_000_000 + 7_500_000_000},
		// 300k plain at $3, 100k audio at $40; the cached 600k has no rate.
		{"the prompt rate charges what its parts left", map[string]int64{"prompt_tokens": 1_000_000, "cached_tokens": 600_000, "audio_tokens": 100_000}, 900_000_000 + 4_000_000_000},
		{"audio output comes out of the completion", map[string]int64{"completion_tokens": 500_000, "completion_audio_tokens": 100_000}, 6_000_000_000 + 8_000_000_000},
		{"parts past the prompt charge no prompt", map[string]int64{"prompt_tokens": 10, "cached_tokens": 50}, 0},
		// Pooled first, 1,999,998 nano-Mtok would round to 1; per counter each is 0.
		{"truncates per counter", map[string]int64{"cache_write_tokens": 999_999, "cache_write_1h_tokens": 999_999}, 0},
		{"audio tokens", map[string]int64{"audio_tokens": 500_000}, 20_000_000_000},
		{"audio seconds are display only", map[string]int64{"audio_seconds": 90}, 0},
		{"per request", map[string]int64{"request_count": 1}, 1_000},
		{"unrated counter is free", map[string]int64{"cached_tokens": 1_000_000}, 0},
		{"twins and display fields are not priced", map[string]int64{"total_tokens": 1_000_000, "m:completion_tokens:m": 1_000_000}, 0},
		{"no rates at all", map[string]int64{"completion_tokens": 1_000_000}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rates := rates
			if tc.name == "no rates at all" {
				rates = nil
			}
			if got := counters(t).Cost(tc.counts, rates); got != tc.want {
				t.Fatalf("Cost = %d, want %d", got, tc.want)
			}
		})
	}
}

// The shipped table is today's twelve: every part names a root, every derived row a root base.
func TestTheShippedCountersAreWellFormed(t *testing.T) {
	table := counters(t)
	if len(table) != 12 {
		t.Fatalf("%d counters", len(table))
	}
	for _, c := range table {
		for _, ref := range []string{c.PartOf, c.Base} {
			if ref == "" {
				continue
			}
			row, known := table.row(ref)
			if !known || row.Base != "" {
				t.Errorf("%s refers to %q, not a root of the table", c.Name, ref)
			}
		}
	}
	if got := table.Parts("prompt_tokens_above_272k"); len(got) != 2 || got[0] != "cached_tokens_above_272k" || got[1] != "cache_write_tokens_above_272k" {
		t.Errorf("parts of prompt_tokens_above_272k = %v", got)
	}
}

// A table with a second bracket: the variant is the highest threshold strictly below the prompt,
// a root with no row at that threshold has none, and a derived row's parts are its base's at the
// same threshold.
func TestVariantPicksTheHighestThresholdBelowThePrompt(t *testing.T) {
	table := append(counters(t),
		Counter{Name: "prompt_tokens_above_500k", Divisor: 1e6, Base: "prompt_tokens", MinPromptTokens: 500000},
		Counter{Name: "cached_tokens_above_500k", Divisor: 1e6, Base: "cached_tokens", MinPromptTokens: 500000},
	)
	cases := []struct {
		root   string
		prompt int64
		want   string
	}{
		{"prompt_tokens", 100000, ""},
		{"prompt_tokens", 272000, ""},
		{"prompt_tokens", 272001, "prompt_tokens_above_272k"},
		{"prompt_tokens", 600000, "prompt_tokens_above_500k"},
		{"cache_write_tokens", 600000, "cache_write_tokens_above_272k"},
		{"cache_write_1h_tokens", 600000, ""},
		{"request_count", 600000, ""},
	}
	for _, tc := range cases {
		if got := table.Variant(tc.root, tc.prompt); got != tc.want {
			t.Errorf("Variant(%s, %d) = %q, want %q", tc.root, tc.prompt, got, tc.want)
		}
	}
	if got := table.Parts("prompt_tokens_above_500k"); len(got) != 1 || got[0] != "cached_tokens_above_500k" {
		t.Errorf("parts of prompt_tokens_above_500k = %v", got)
	}
}

// The table rides inside the pricing as the control plane pushes it; a pricing pushed without one,
// or no pricing at all, is an empty table.
func TestAPricingCarriesItsTable(t *testing.T) {
	var pushed Pricing
	raw := `{"id":"p1","rates":{"prompt_tokens":3000000000},"counters":[
		{"name":"prompt_tokens","divisor":1000000},
		{"name":"cached_tokens","divisor":1000000,"part_of":"prompt_tokens"},
		{"name":"prompt_tokens_above_200k","divisor":1000000,"base":"prompt_tokens","min_prompt_tokens":200000},
		{"name":"request_count","divisor":1}]}`
	if err := json.Unmarshal([]byte(raw), &pushed); err != nil {
		t.Fatal(err)
	}
	if got := pushed.Table(); len(got) != 4 || got[2].Base != "prompt_tokens" || got[2].MinPromptTokens != 200000 {
		t.Errorf("table = %+v", got)
	}
	if got := pushed.Table().Variant("prompt_tokens", 250000); got != "prompt_tokens_above_200k" {
		t.Errorf("Variant = %q", got)
	}
	bare := &Pricing{ID: "p2", Rates: map[string]int64{"prompt_tokens": 1}}
	if len(bare.Table()) != 0 || len((*Pricing)(nil).Table()) != 0 {
		t.Error("a pricing without a table, or no pricing, is not empty")
	}
	if got := bare.Table().Cost(map[string]int64{"prompt_tokens": 1e6}, bare.Rates); got != 0 {
		t.Errorf("an empty table priced %d", got)
	}
	// A counter the table does not name is not priced, whatever the rates say.
	if got := pushed.Table().Cost(map[string]int64{"completion_tokens": 1e6}, map[string]int64{"completion_tokens": 15e9}); got != 0 {
		t.Errorf("Cost of an unlisted counter = %d", got)
	}
}

// An above-272k counter is charged at its own rate, or at its base counter's when the pricing
// holds none for it.
func TestRate(t *testing.T) {
	rates := map[string]int64{
		"prompt_tokens": 2.5e9, "prompt_tokens_above_272k": 5e9, "cached_tokens": 0.25e9,
		"completion_tokens": 15e9, "completion_tokens_above_272k": 0,
	}
	cases := []struct {
		name    string
		counter string
		rates   map[string]int64
		want    int64
	}{
		{"its own rate", "prompt_tokens_above_272k", rates, 5e9},
		{"none of its own: its base counter's", "cached_tokens_above_272k", rates, 0.25e9},
		{"a rate of 0 is a rate", "completion_tokens_above_272k", rates, 0},
		{"a base counter has nothing to fall back to", "audio_tokens", rates, 0},
		{"no rates at all", "prompt_tokens_above_272k", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := counters(t).Rate(tc.counter, tc.rates); got != tc.want {
				t.Fatalf("Rate = %d, want %d", got, tc.want)
			}
		})
	}
}

// Prompt and completion at their above-272k rates; cached has none, so at its base rate.
func TestCostAbove272k(t *testing.T) {
	rates := map[string]int64{
		"prompt_tokens": 2.5e9, "cached_tokens": 0.25e9, "completion_tokens": 15e9,
		"prompt_tokens_above_272k": 5e9, "completion_tokens_above_272k": 22.5e9,
	}
	counts := map[string]int64{
		"prompt_tokens_above_272k": 300000, "cached_tokens_above_272k": 280000, "completion_tokens_above_272k": 1000,
	}
	if got := counters(t).Cost(counts, rates); got != 100_000_000+70_000_000+22_500_000 {
		t.Errorf("Cost = %d", got)
	}
}
