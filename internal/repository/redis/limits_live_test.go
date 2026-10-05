//go:build live

package redis

import (
	"context"
	"testing"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// The script admits up to the ceiling, counts nothing for a refusal, names every spent limit, and a
// debit lands on the token counter alone — each counter with a TTL of two windows.
func TestLimitsAgainstRealRedis(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	limits := client.Store().Limits
	at := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	requests := domain.Limit{Metric: domain.LimitRequests, Window: "1m", Value: 2}
	tokens := domain.Limit{Metric: domain.LimitTokens, Window: "1h", Value: 200}
	both := []domain.Limit{requests, tokens}

	for i := 0; i < 2; i++ {
		if exceeded, err := limits.Admit(ctx, "GU-1", both, at); err != nil || len(exceeded) != 0 {
			t.Fatalf("admit %d = %+v, %v", i+1, exceeded, err)
		}
	}
	exceeded, err := limits.Admit(ctx, "GU-1", both, at)
	if err != nil || len(exceeded) != 1 || exceeded[0] != requests {
		t.Fatalf("third = %+v, %v; want the request limit", exceeded, err)
	}
	label, _, _ := requests.Bucket(at)
	key := limitKey("GU-1", requests, label)
	if n, _ := client.rdb.Get(ctx, key).Int(); n != 2 {
		t.Errorf("%s = %d, want 2: a refusal counts nothing", key, n)
	}
	if ttl := client.rdb.TTL(ctx, key).Val(); ttl <= time.Minute || ttl > 2*time.Minute {
		t.Errorf("request counter ttl = %s, want two windows", ttl)
	}

	if err := limits.Debit(ctx, "GU-1", []domain.Limit{tokens}, 240, at); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	label, _, _ = tokens.Bucket(at)
	key = limitKey("GU-1", tokens, label)
	if n, _ := client.rdb.Get(ctx, key).Int(); n != 240 {
		t.Errorf("%s = %d, want 240", key, n)
	}
	if ttl := client.rdb.TTL(ctx, key).Val(); ttl <= time.Hour || ttl > 2*time.Hour {
		t.Errorf("token counter ttl = %s, want two windows", ttl)
	}
	// The next minute has room for requests again, but the hour's tokens are spent.
	exceeded, err = limits.Admit(ctx, "GU-1", both, at.Add(time.Minute))
	if err != nil || len(exceeded) != 1 || exceeded[0] != tokens {
		t.Fatalf("next minute = %+v, %v; want the token limit", exceeded, err)
	}
}

// Limits ride the user record: a push writes them, a blank push clears them, and the record reads
// back parsed.
func TestAPushWritesAndClearsLimitsAgainstRealRedis(t *testing.T) {
	client, state := liveStore(t)
	ctx := context.Background()
	push := func(limits string) {
		t.Helper()
		_, err := state.Apply(ctx, repository.StatePush{Users: map[string]repository.UserBucket{
			domain.BucketOf("GU-1"): {Hash: "uh", Records: []repository.UserUpsert{{Name: "GU-1", Limits: limits}}},
		}})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	push("requests:1m:200")
	if usr, _, err := client.Store().Users.Get(ctx, "GU-1"); err != nil || len(usr.Limits) != 1 || usr.Limits[0].Value != 200 {
		t.Fatalf("user = %+v, %v", usr, err)
	}
	push("")
	if usr, _, err := client.Store().Users.Get(ctx, "GU-1"); err != nil || len(usr.Limits) != 0 {
		t.Fatalf("after a blank push: %+v, %v", usr, err)
	}
	// A limit a newer binary wrote is an error, not a holder served uncapped.
	client.rdb.HSet(ctx, "user:GU-1", "limits", "concurrent:1m:5")
	if _, _, err := client.Store().Users.Get(ctx, "GU-1"); err == nil {
		t.Error("an unreadable limit read as no limit")
	}
}
