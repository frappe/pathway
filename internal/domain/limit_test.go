package domain

import (
	"strconv"
	"testing"
	"time"
)

func TestParseLimits(t *testing.T) {
	limits, err := ParseLimits("requests:1m:200, total_tokens:1M:50000, requests:1m:10:openai/gpt-6-luna")
	want := []Limit{{"requests", "1m", 200, ""}, {"total_tokens", "1M", 50000, ""}, {"requests", "1m", 10, "openai/gpt-6-luna"}}
	if err != nil || len(limits) != 3 || limits[0] != want[0] || limits[1] != want[1] || limits[2] != want[2] {
		t.Fatalf("limits = %+v, err = %v", limits, err)
	}
	// The model is the last part, so one holding a colon still reads whole.
	if limits, err := ParseLimits("requests:1m:1:ns:model"); err != nil || limits[0].Model != "ns:model" {
		t.Errorf("colon in the model = %+v, %v", limits, err)
	}
	if limits, err := ParseLimits(""); err != nil || limits != nil {
		t.Errorf("blank = %+v, %v; want none", limits, err)
	}
	for _, bad := range []string{
		"requests:1m", "requests:1m:0", "requests:1m:x", "requests:5m:10",
		"input_tokens:1m:10", "requests:1m:10:",
	} {
		if _, err := ParseLimits(bad); err == nil {
			t.Errorf("ParseLimits(%q) read; want an error", bad)
		}
	}
}

// A row naming the model stands in for the blank one on its metric and window; the other metric
// keeps the blank row; a model no row reaches has no limit at all.
func TestLimitsForPrefersTheRowNamingTheModel(t *testing.T) {
	rows := []Limit{
		{LimitRequests, "1m", 60, ""}, {LimitTokens, "1m", 300000, ""},
		{LimitRequests, "1m", 10, "luna"}, {LimitRequests, "1h", 5, "sol"},
	}
	if luna := LimitsFor(rows, "luna"); len(luna) != 2 || luna[0] != rows[2] || luna[1] != rows[1] {
		t.Errorf("luna = %+v", luna)
	}
	if sol := LimitsFor(rows, "sol"); len(sol) != 3 || sol[0] != rows[3] || sol[1] != rows[0] || sol[2] != rows[1] {
		t.Errorf("sol = %+v, want its hour row and both blank rows", sol)
	}
	if other := LimitsFor(rows, "other"); len(other) != 2 || other[0] != rows[0] {
		t.Errorf("other = %+v, want the blank rows", other)
	}
	if none := LimitsFor(rows[2:3], "other"); none != nil {
		t.Errorf("a model no row names = %+v, want none", none)
	}
}

// The team's rows and the key's own are two holders, and one that names nothing for the model is
// not counted at all.
func TestHoldersSkipsWhatNamesNothing(t *testing.T) {
	key := KeyRecord{
		Team: "T-1", KeyPrefix: "abc",
		Limits:    []Limit{{LimitRequests, "1m", 60, ""}},
		KeyLimits: []Limit{{LimitRequests, "1m", 5, "luna"}},
	}
	luna := Holders(key, "luna")
	if len(luna) != 2 || luna[0].Counter() != "team:T-1:luna" || luna[1].Counter() != "key:abc:luna" || luna[1].Limits[0].Value != 5 {
		t.Errorf("luna = %+v", luna)
	}
	if other := Holders(key, "other"); len(other) != 1 || other[0].Kind != HolderTeam {
		t.Errorf("other = %+v, want the team alone", other)
	}
	if none := Holders(KeyRecord{Team: "T-1"}, "luna"); none != nil {
		t.Errorf("no rows = %+v, want none", none)
	}
}

// The last second of a year is the last second of every window, and the next one starts them all.
func TestBucketResetsOnTheUTCClock(t *testing.T) {
	at := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	cases := []struct {
		window, label string
		ttl           int
	}{
		{"1m", strconv.FormatInt(at.Unix()/60, 10), 120},
		{"1h", strconv.FormatInt(at.Unix()/3600, 10), 7200},
		{"1d", strconv.FormatInt(at.Unix()/86400, 10), 172800},
		{"1M", "2026-12", 62 * 86400},
	}
	for _, tc := range cases {
		limit := Limit{Metric: LimitRequests, Window: tc.window, Value: 1}
		label, resetIn, ttl := limit.Bucket(at)
		if label != tc.label || resetIn != 1 || ttl != tc.ttl {
			t.Errorf("%s: bucket = %q, reset in %d, ttl %d; want %q, 1, %d", tc.window, label, resetIn, ttl, tc.label, tc.ttl)
		}
		if next, _, _ := limit.Bucket(at.Add(time.Second)); next == label {
			t.Errorf("%s: the next second is still bucket %q", tc.window, label)
		}
		// The same instant on a clock ahead of UTC, where it is already next year.
		if elsewhere, _, _ := limit.Bucket(at.In(time.FixedZone("IST", 19800))); elsewhere != label {
			t.Errorf("%s: bucket = %q off a +05:30 clock, want %q", tc.window, elsewhere, label)
		}
	}
}

func TestAMonthResetsOnTheFirst(t *testing.T) {
	at := time.Date(2026, 2, 27, 0, 0, 0, 0, time.UTC)
	label, resetIn, _ := Limit{Metric: LimitTokens, Window: "1M", Value: 1}.Bucket(at)
	if label != "2026-02" || resetIn != 2*86400 {
		t.Errorf("bucket = %q, reset in %d; want 2026-02, %d", label, resetIn, 2*86400)
	}
}

// Waiting out the minute would only meet the hour's limit, so the hour is the one named.
func TestLimitDenialNamesTheLimitThatResetsLast(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	denial := LimitDenial([]Over{
		{Holder{Kind: HolderTeam, ID: "T-1", Model: "luna"}, Limit{LimitRequests, "1m", 200, ""}},
		{Holder{Kind: HolderKey, ID: "abc", Model: "luna"}, Limit{LimitTokens, "1h", 50000, ""}},
	}, at)
	if denial.Status != 429 || denial.Reason != "rate limit exceeded: 50000 tokens per 1h on luna (key)" || denial.RetryAfter != 3570 {
		t.Errorf("denial = %+v", denial)
	}
}
