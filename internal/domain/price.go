package domain

// Counter is one row of the control plane's counter table, pushed inside every pricing. A root
// counter is a bucket the gateway fills from the response; a derived one (Base set) is counted
// instead of its base when the request's whole prompt exceeds MinPromptTokens, and bills at the
// base's rate when the pricing holds none of its own. PartOf names the root this one is carved out
// of: the container's rate charges what its parts left of it.
type Counter struct {
	Name            string `json:"name"`
	Divisor         int64  `json:"divisor"`
	PartOf          string `json:"part_of,omitempty"`
	Base            string `json:"base,omitempty"`
	MinPromptTokens int64  `json:"min_prompt_tokens,omitempty"`
}

// CounterTable is every counter a pricing names, in the control plane's order. Twelve rows today
// (grove/catalog/catalog.json, mirrored in testdata/counters.json); each lookup is a scan, which is
// cheaper than a map for a table this size.
type CounterTable []Counter

// Table is the counter table a pricing carries. No pricing, or one pushed without a table, is an
// empty table: every bucket is counted under its root and nothing is priced.
func (p *Pricing) Table() CounterTable {
	if p == nil {
		return nil
	}
	return p.Counters
}

func (t CounterTable) row(name string) (Counter, bool) {
	for _, c := range t {
		if c.Name == name {
			return c, true
		}
	}
	return Counter{}, false
}

// Variant is the derived counter root is counted under for a request of this prompt size: the
// one with the highest threshold strictly below the prompt, or "" when none applies.
func (t CounterTable) Variant(root string, prompt int64) string {
	var pick Counter
	for _, c := range t {
		if c.Base == root && prompt > c.MinPromptTokens && c.MinPromptTokens >= pick.MinPromptTokens {
			pick = c
		}
	}
	return pick.Name
}

// Parts is the counters charged out of this one. A root's are the rows naming it as PartOf; a
// derived counter's are its base's parts at its own threshold — the ones that moved with it.
func (t CounterTable) Parts(counter string) []string {
	row, known := t.row(counter)
	if !known {
		return nil
	}
	var parts []string
	if row.Base == "" {
		for _, c := range t {
			if c.PartOf == counter {
				parts = append(parts, c.Name)
			}
		}
		return parts
	}
	for _, base := range t.Parts(row.Base) {
		for _, c := range t {
			if c.Base == base && c.MinPromptTokens == row.MinPromptTokens {
				parts = append(parts, c.Name)
			}
		}
	}
	return parts
}

// Rate is what a counter is charged at: its own rate, or, for a derived counter the pricing holds
// no rate for, its base counter's. A rate of 0 is a rate.
func (t CounterTable) Rate(counter string, rates map[string]int64) int64 {
	if rate, held := rates[counter]; held {
		return rate
	}
	row, _ := t.row(counter)
	return rates[row.Base]
}

// Cost prices one request in nano-USD: Σ amount × rate / divisor, truncated per counter, where a
// counter's amount is what its parts left of it. A counter without a rate costs nothing — the
// control plane, not this box, decides what an unpriced quantity means. Worst case amount × rate is
// around 1e17, inside int64.
func (t CounterTable) Cost(counts, rates map[string]int64) int64 {
	var total int64
	for _, c := range t {
		amount, rate := counts[c.Name], t.Rate(c.Name, rates)
		for _, part := range t.Parts(c.Name) {
			amount -= counts[part]
		}
		if amount <= 0 || rate <= 0 {
			continue
		}
		total += amount * rate / c.Divisor
	}
	return total
}
