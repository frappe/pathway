package domain

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewRequestID(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	first := NewRequestID()
	if !shape.MatchString(first) {
		t.Fatalf("%q is not a uuid v7", first)
	}

	// The leading 48 bits are unix milliseconds: what makes the id sort by admission.
	ms, err := strconv.ParseInt(strings.ReplaceAll(first[:13], "-", ""), 16, 64)
	if err != nil {
		t.Fatalf("timestamp part of %q: %v", first, err)
	}
	if age := time.Since(time.UnixMilli(ms)); age < 0 || age > time.Second {
		t.Errorf("%q was stamped %v ago, want about now", first, age)
	}

	time.Sleep(2 * time.Millisecond)
	second := NewRequestID()
	if second == first {
		t.Error("two ids should differ")
	}
	if !(second > first) {
		t.Errorf("a later id should sort after an earlier one: %q !> %q", second, first)
	}
}
