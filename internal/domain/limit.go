package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// What a Limit counts. A request is counted as it is admitted; tokens only once the answer says how
// many there were.
// ponytail: nothing is reserved at admit, so requests in flight together pass a token limit by what
// they go on to use. That much is taken out of the next window (Limits.Debit), one window on and no
// further. Reserve an estimate of the prompt in Admit and settle it in Debit if the burst itself hurts.
const (
	LimitRequests = "requests"
	LimitTokens   = "total_tokens"
)

// limitWindows is every window a limit may reset on, in seconds. The month is the calendar's.
var limitWindows = map[string]int64{"1m": 60, "1h": 3600, "1d": 86400, "1M": 0}

// Limit caps one metric over one window, pushed as "<metric>:<window>:<value>" or, for one model
// alone, "<metric>:<window>:<value>:<model>". A blank Model is the holder's default on every
// model; a row naming one stands in for it there (LimitsFor). A Value of 0 is no limit: the row
// still stands in, and counts nothing. A window resets on the UTC clock —
// top of the minute, the hour, midnight, the 1st — not from the holder's first request.
// ponytail: a fixed window lets twice the ceiling through across its boundary. The last bucket is
// kept two windows (Bucket): weigh it into the check if bursts hurt.
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
		if err != nil || value < 0 || !known || (parts[0] != LimitRequests && parts[0] != LimitTokens) {
			return nil, fmt.Errorf("limit %q: unknown metric or window, or a value below zero", entry)
		}
		limit := Limit{Metric: parts[0], Window: parts[1], Value: value}
		if len(parts) == 4 {
			limit.Model = parts[3]
		}
		limits = append(limits, limit)
	}
	return limits, nil
}

// ParseKeyLimits reads a key's limits: ParseLimits, and a row naming a model is an error, since a
// key's limits count every model together and it would be enforced nowhere.
func ParseKeyLimits(list string) ([]Limit, error) {
	limits, err := ParseLimits(list)
	for _, limit := range limits {
		if limit.Model != "" {
			return nil, fmt.Errorf("key limit %s:%s names a model (%s); a key's limits name none", limit.Metric, limit.Window, limit.Model)
		}
	}
	return limits, err
}

// LimitsFor is what counts on model out of a holder's rows: the rows naming it, and on every metric
// and window none of those covers, the rows naming no model. A row at 0 covers its metric and
// window and counts nothing. No row for the model = no limit on it.
func LimitsFor(limits []Limit, model string) []Limit {
	var on []Limit
	named := map[[2]string]bool{}
	for _, limit := range limits {
		if limit.Model == model {
			named[[2]string{limit.Metric, limit.Window}] = true
			if limit.Value > 0 {
				on = append(on, limit)
			}
		}
	}
	for _, limit := range limits {
		if limit.Model == "" && limit.Value > 0 && !named[[2]string{limit.Metric, limit.Window}] {
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

// Next is the start of the window after the one now falls in.
func (l Limit) Next(now time.Time) time.Time {
	_, resetIn, _ := l.Bucket(now)
	return time.Unix(now.Unix()+int64(resetIn), 0)
}

// Whose limits a request is counted against. The team's rows count on lim:team:<team>:<model>:…,
// so every key of the team draws on one window per model; the key's on lim:key:<prefix>:…, every
// model together. Whichever is lower refuses first.
const (
	HolderTeam = "team"
	HolderKey  = "key"
)

// Holder is one set of limits a request is counted against. Model is the one asked for, or blank
// for a key: its limits name no model.
type Holder struct {
	Kind   string // HolderTeam | HolderKey
	ID     string // the Grove Team, or the key prefix
	Model  string
	Limits []Limit
}

// Counter is the holder's part of its counter keys: "team:<team>:<model>", or "key:<prefix>".
func (h Holder) Counter() string {
	if h.Model == "" {
		return h.Kind + ":" + h.ID
	}
	return h.Kind + ":" + h.ID + ":" + h.Model
}

// Holders is what counts for key on model: the team's rows resolved for the model, and the key's,
// the rows naming no model, which count every model together. Only those that name something.
// None = nothing to count, no store call.
func Holders(key KeyRecord, model string) []Holder {
	holders := FallbackHolders(key, model)
	if own := LimitsFor(key.KeyLimits, ""); len(own) > 0 {
		holders = append(holders, Holder{Kind: HolderKey, ID: key.KeyPrefix, Limits: own})
	}
	return holders
}

// FallbackHolders is what a fallback to model counts on top of the request's own admission: the
// team's rows for that model. The key's limits were counted when the request was admitted.
func FallbackHolders(key KeyRecord, model string) []Holder {
	if on := LimitsFor(key.Limits, model); len(on) > 0 {
		return []Holder{{Kind: HolderTeam, ID: key.Team, Model: model, Limits: on}}
	}
	return nil
}

// Over is a limit found at its ceiling, and whose it is. Carried: the window after is spent as
// well, by what this one ran over.
type Over struct {
	Holder  Holder
	Limit   Limit
	Carried bool
}

// LimitDenial is the 429 for a request over these limits. It names the one that resets last, so a
// client that waits out Retry-After does not walk into the next, and says whose it is.
func LimitDenial(exceeded []Over, now time.Time) Denial {
	var last Over
	wait := 0
	for _, over := range exceeded {
		_, resetIn, _ := over.Limit.Bucket(now)
		if over.Carried {
			_, next, _ := over.Limit.Bucket(over.Limit.Next(now))
			resetIn += next
		}
		if resetIn > wait {
			last, wait = over, resetIn
		}
	}
	unit := "requests"
	if last.Limit.Metric == LimitTokens {
		unit = "tokens"
	}
	on := ""
	if last.Holder.Model != "" {
		on = " on " + last.Holder.Model
	}
	return Denial{
		Status: 429,
		Reason: fmt.Sprintf("rate limit exceeded: %d %s per %s%s (%s)",
			last.Limit.Value, unit, last.Limit.Window, on, last.Holder.Kind),
		RetryAfter: wait,
	}
}
