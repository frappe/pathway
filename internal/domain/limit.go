package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// What a Limit counts. A request is counted as it is admitted; tokens only once the answer says how
// many there were.
const (
	LimitRequests = "requests"
	LimitTokens   = "total_tokens"
)

// limitWindows is every window a limit may reset on, in seconds. The month is the calendar's.
var limitWindows = map[string]int64{"1m": 60, "1h": 3600, "1d": 86400, "1M": 0}

// Limit caps one metric over one window, pushed on the holder as "<metric>:<window>:<value>". A
// window resets on the UTC clock — top of the minute, the hour, midnight, the 1st — not from the
// holder's first request.
type Limit struct {
	Metric string
	Window string
	Value  int64
}

// ParseLimits reads the comma list a push carries. An entry it cannot read is an error, never a
// limit quietly not enforced.
func ParseLimits(list string) ([]Limit, error) {
	var limits []Limit
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("limit %q: want metric:window:value", entry)
		}
		value, err := strconv.ParseInt(parts[2], 10, 64)
		_, known := limitWindows[parts[1]]
		if err != nil || value <= 0 || !known || (parts[0] != LimitRequests && parts[0] != LimitTokens) {
			return nil, fmt.Errorf("limit %q: unknown metric or window, or a value not above zero", entry)
		}
		limits = append(limits, Limit{Metric: parts[0], Window: parts[1], Value: value})
	}
	return limits, nil
}

// LimitsOn is the limits that count metric.
func LimitsOn(limits []Limit, metric string) []Limit {
	var on []Limit
	for _, limit := range limits {
		if limit.Metric == metric {
			on = append(on, limit)
		}
	}
	return on
}

// Bucket is the window now falls in: its label, the seconds until it ends, and how long its counter
// is kept, which is two windows.
func (l Limit) Bucket(now time.Time) (label string, resetIn, ttl int) {
	now = now.UTC()
	if l.Window == "1M" {
		next := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		return now.Format("2006-01"), int(next.Unix() - now.Unix()), 62 * 86400
	}
	seconds := limitWindows[l.Window]
	bucket := now.Unix() / seconds
	return strconv.FormatInt(bucket, 10), int((bucket+1)*seconds - now.Unix()), int(2 * seconds)
}

// LimitDenial is the 429 for a holder over these limits. It names the one that resets last, so a
// client that waits out Retry-After does not walk into the next.
func LimitDenial(exceeded []Limit, now time.Time) Denial {
	var last Limit
	wait := 0
	for _, limit := range exceeded {
		if _, resetIn, _ := limit.Bucket(now); resetIn > wait {
			last, wait = limit, resetIn
		}
	}
	unit := "requests"
	if last.Metric == LimitTokens {
		unit = "tokens"
	}
	return Denial{
		Status:     429,
		Reason:     fmt.Sprintf("rate limit exceeded: %d %s per %s", last.Value, unit, last.Window),
		RetryAfter: wait,
	}
}
