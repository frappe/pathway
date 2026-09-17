package redis

import "testing"

// A shared store refuses a client without its password, and loopback Redis has none to give.
func TestNewCarriesThePassword(t *testing.T) {
	if got := New("store:6379", "s3cret").rdb.Options().Password; got != "s3cret" {
		t.Errorf("password = %q, want %q", got, "s3cret")
	}
	if got := New("127.0.0.1:6379", "").rdb.Options().Password; got != "" {
		t.Errorf("a loopback client sent password %q", got)
	}
}
