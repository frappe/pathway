package domain

import "testing"

// Truncation is per counter, so a divisor never rounds one counter's remainder into another's,
// and a counter the route does not price contributes nothing rather than failing the sum.
func TestCost(t *testing.T) {
	// $3/Mtok, $15/Mtok, $40/Mtok of audio, $0.000001/request, and one nano-USD per Mtok written.
	rates := map[string]int64{
		"input_tokens": 3e9, "completion_tokens": 15e9, "audio_tokens": 40e9, "request_count": 1_000,
		"cache_write_tokens": 1, "cache_write_1h_tokens": 1,
	}
	cases := []struct {
		name   string
		counts map[string]int64
		want   int64
	}{
		{"prompt and completion", map[string]int64{"input_tokens": 1_000_000, "completion_tokens": 500_000}, 3_000_000_000 + 7_500_000_000},
		// Pooled first, 1,999,998 nano-Mtok would round to 1; per counter each is 0.
		{"truncates per counter", map[string]int64{"cache_write_tokens": 999_999, "cache_write_1h_tokens": 999_999}, 0},
		{"audio tokens", map[string]int64{"audio_tokens": 500_000}, 20_000_000_000},
		{"audio seconds are display only", map[string]int64{"audio_seconds": 90}, 0},
		{"per request", map[string]int64{"request_count": 1}, 1_000},
		{"unrated counter is free", map[string]int64{"cached_tokens": 1_000_000}, 0},
		{"twins and display fields are not priced", map[string]int64{"prompt_tokens": 1_000_000, "m:completion_tokens:m": 1_000_000}, 0},
		{"no rates at all", map[string]int64{"completion_tokens": 1_000_000}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rates := rates
			if tc.name == "no rates at all" {
				rates = nil
			}
			if got := Cost(tc.counts, rates); got != tc.want {
				t.Fatalf("Cost = %d, want %d", got, tc.want)
			}
		})
	}
}

// The counters tagged per pricing are exactly the ones Cost prices.
func TestPricedCountersMatchTheDivisors(t *testing.T) {
	if len(PricedCounters) != len(divisors) {
		t.Fatalf("%d priced counters, %d divisors", len(PricedCounters), len(divisors))
	}
	for _, counter := range PricedCounters {
		if divisors[counter] == 0 {
			t.Errorf("%s has no divisor", counter)
		}
	}
}
