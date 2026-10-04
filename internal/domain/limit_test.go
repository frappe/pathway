package domain

import (
	"strconv"
	"testing"
	"time"
)

func TestParseLimits(t *testing.T) {
	limits, err := ParseLimits("requests:1m:200, total_tokens:1M:50000")
	want := []Limit{{"requests", "1m", 200}, {"total_tokens", "1M", 50000}}
	if err != nil || len(limits) != 2 || limits[0] != want[0] || limits[1] != want[1] {
		t.Fatalf("limits = %+v, err = %v", limits, err)
	}
	if limits, err := ParseLimits(""); err != nil || limits != nil {
		t.Errorf("blank = %+v, %v; want none", limits, err)
	}
	for _, bad := range []string{
		"requests:1m", "requests:1m:0", "requests:1m:x", "requests:5m:10",
		"input_tokens:1m:10", "requests:1m:10:extra",
	} {
		if _, err := ParseLimits(bad); err == nil {
			t.Errorf("ParseLimits(%q) read; want an error", bad)
		}
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
	denial := LimitDenial([]Limit{
		{LimitRequests, "1m", 200}, {LimitTokens, "1h", 50000},
	}, at)
	if denial.Status != 429 || denial.Reason != "rate limit exceeded: 50000 tokens per 1h" || denial.RetryAfter != 3570 {
		t.Errorf("denial = %+v", denial)
	}
}
