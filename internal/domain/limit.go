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

// Limit caps one metric over one window, pushed as "<metric>:<window>:<value>" or, for one model
// alone, "<metric>:<window>:<value>:<model>". A blank Model is the holder's default on every
// model; a row naming one stands in for it there (LimitsFor). A window resets on the UTC clock —
// top of the minute, the hour, midnight, the 1st — not from the holder's first request.
type Limit struct {
	Metric string
	Window string
	Value  int64
	Model  string
}

// ParseLimits reads the comma list a push carries. An entry it cannot read is an error, never a
// limit quietly not enforced. The model comes last, so a model id holding a colon still splits.
func ParseLimits(list string) ([]Limit, error) {
	var limits []Limit
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 4)
		if len(parts) < 3 || (len(parts) == 4 && parts[3] == "") {
			return nil, fmt.Errorf("limit %q: want metric:window:value[:model]", entry)
		}
		value, err := strconv.ParseInt(parts[2], 10, 64)
		_, known := limitWindows[parts[1]]
		if err != nil || value <= 0 || !known || (parts[0] != LimitRequests && parts[0] != LimitTokens) {
			return nil, fmt.Errorf("limit %q: unknown metric or window, or a value not above zero", entry)
		}
		limit := Limit{Metric: parts[0], Window: parts[1], Value: value}
		if len(parts) == 4 {
			limit.Model = parts[3]
		}
		limits = append(limits, limit)
	}
	return limits, nil
}

// LimitsFor is what counts on model out of a holder's rows: the rows naming it, and on every metric
// and window none of those covers, the rows naming no model. No row for the model = no limit on it.
func LimitsFor(limits []Limit, model string) []Limit {
	var on []Limit
	named := map[[2]string]bool{}
	for _, limit := range limits {
		if limit.Model == model {
			on = append(on, limit)
			named[[2]string{limit.Metric, limit.Window}] = true
		}
	}
	for _, limit := range limits {
		if limit.Model == "" && !named[[2]string{limit.Metric, limit.Window}] {
			on = append(on, limit)
		}
	}
	return on
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

// Whose limits a request is counted against. The team's rows count on lim:team:<team>:<model>:…,
// so every key of the team draws on one window; the key's own on lim:key:<prefix>:<model>:…, a
// second gate beneath the team's. Whichever is lower refuses first.
const (
	HolderTeam = "team"
	HolderKey  = "key"
)

// Holder is one set of limits a request is counted against, resolved for the model asked for.
type Holder struct {
	Kind   string // HolderTeam | HolderKey
	ID     string // the Grove Team, or the key prefix
	Model  string
	Limits []Limit
}

// Counter is the holder's part of its counter keys: "<kind>:<id>:<model>".
func (h Holder) Counter() string { return h.Kind + ":" + h.ID + ":" + h.Model }

// Holders is what counts for key on model: the team's rows and the key's own, each resolved for
// the model, and only those that name something. None = nothing to count, no store call.
func Holders(key KeyRecord, model string) []Holder {
	var holders []Holder
	for _, h := range []Holder{
		{Kind: HolderTeam, ID: key.Team, Model: model, Limits: LimitsFor(key.Limits, model)},
		{Kind: HolderKey, ID: key.KeyPrefix, Model: model, Limits: LimitsFor(key.KeyLimits, model)},
	} {
		if len(h.Limits) > 0 {
			holders = append(holders, h)
		}
	}
	return holders
}

// Over is a limit found at its ceiling, and whose it is.
type Over struct {
	Holder Holder
	Limit  Limit
}

// LimitDenial is the 429 for a request over these limits. It names the one that resets last, so a
// client that waits out Retry-After does not walk into the next, and says whose it is.
func LimitDenial(exceeded []Over, now time.Time) Denial {
	var last Over
	wait := 0
	for _, over := range exceeded {
		if _, resetIn, _ := over.Limit.Bucket(now); resetIn > wait {
			last, wait = over, resetIn
		}
	}
	unit := "requests"
	if last.Limit.Metric == LimitTokens {
		unit = "tokens"
	}
	return Denial{
		Status: 429,
		Reason: fmt.Sprintf("rate limit exceeded: %d %s per %s on %s (%s)",
			last.Limit.Value, unit, last.Limit.Window, last.Holder.Model, last.Holder.Kind),
		RetryAfter: wait,
	}
}
