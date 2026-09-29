package domain

// divisors turn a counter's amount into the unit its rate is quoted per: tokens per Mtok, requests
// each. The same seven counters, with the same divisors, live in grove/pricing.py.
var divisors = map[string]int64{
	"input_tokens":          1e6,
	"cached_tokens":         1e6,
	"cache_write_tokens":    1e6,
	"cache_write_1h_tokens": 1e6,
	"completion_tokens":     1e6,
	"audio_tokens":          1e6,
	"request_count":         1,
}

// PricedCounters is the seven counters a rate can name, in a fixed order.
var PricedCounters = []string{
	"input_tokens", "cached_tokens", "cache_write_tokens", "cache_write_1h_tokens",
	"completion_tokens", "audio_tokens", "request_count",
}

// Cost prices one request in nano-USD: Σ amount × rate / divisor, truncated per counter. A counter
// without a rate costs nothing — the control plane, not this box, decides what an unpriced quantity
// means. Worst case amount × rate is around 1e17, inside int64.
func Cost(counts, rates map[string]int64) int64 {
	var total int64
	for counter, divisor := range divisors {
		amount, rate := counts[counter], rates[counter]
		if amount <= 0 || rate <= 0 {
			continue
		}
		total += amount * rate / divisor
	}
	return total
}
