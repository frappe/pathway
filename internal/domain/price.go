package domain

// LongContextTokens is the prompt size past which a request is counted under the above-272k
// counters. Strictly past: a prompt of exactly this many tokens is counted under the base ones.
const LongContextTokens = 272000

// longContextCounters is each counter a long prompt is counted under, and the base counter whose
// rate it falls back to. The same table lives in grove/pricing.py.
var longContextCounters = map[string]string{
	"input_tokens_above_272k":       "input_tokens",
	"cached_tokens_above_272k":      "cached_tokens",
	"cache_write_tokens_above_272k": "cache_write_tokens",
	"completion_tokens_above_272k":  "completion_tokens",
}

// divisors turn a counter's amount into the unit its rate is quoted per: tokens per Mtok, requests
// each. The same eleven counters, with the same divisors, live in grove/pricing.py.
var divisors = map[string]int64{
	"input_tokens":                  1e6,
	"cached_tokens":                 1e6,
	"cache_write_tokens":            1e6,
	"cache_write_1h_tokens":         1e6,
	"completion_tokens":             1e6,
	"audio_tokens":                  1e6,
	"input_tokens_above_272k":       1e6,
	"cached_tokens_above_272k":      1e6,
	"cache_write_tokens_above_272k": 1e6,
	"completion_tokens_above_272k":  1e6,
	"request_count":                 1,
}

// PricedCounters is the eleven counters a rate can name, in a fixed order.
var PricedCounters = []string{
	"input_tokens", "cached_tokens", "cache_write_tokens", "cache_write_1h_tokens",
	"completion_tokens", "audio_tokens",
	"input_tokens_above_272k", "cached_tokens_above_272k", "cache_write_tokens_above_272k",
	"completion_tokens_above_272k",
	"request_count",
}

// Rate is what a counter is charged at: its own rate, or, for an above-272k counter the pricing
// holds no rate for, its base counter's. A rate of 0 is a rate.
func Rate(counter string, rates map[string]int64) int64 {
	if rate, held := rates[counter]; held {
		return rate
	}
	return rates[longContextCounters[counter]]
}

// Cost prices one request in nano-USD: Σ amount × rate / divisor, truncated per counter. A counter
// without a rate costs nothing — the control plane, not this box, decides what an unpriced quantity
// means. Worst case amount × rate is around 1e17, inside int64.
func Cost(counts, rates map[string]int64) int64 {
	var total int64
	for counter, divisor := range divisors {
		amount, rate := counts[counter], Rate(counter, rates)
		if amount <= 0 || rate <= 0 {
			continue
		}
		total += amount * rate / divisor
	}
	return total
}
