package metering

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository/memory"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const openAIUsage = `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,` +
	`"prompt_tokens_details":{"cached_tokens":80}}}`

// Every metric is written flat and twinned per model and per deployment in the same hash, so one
// drain carries the aggregate and the breakdown together.
func TestUsageFieldsTwinsEveryMetric(t *testing.T) {
	fields, _ := UsageFields(Report{Model: "qwen3-4b", Deployment: "MD-00007", Usage: openAIUsage})

	for field, want := range map[string]int64{
		"request_count":                1,
		"prompt_tokens":                100,
		"completion_tokens":            20,
		"total_tokens":                 120,
		"cached_tokens":                80,
		"m:total_tokens:qwen3-4b":      120,
		"m:total_tokens:MD-00007":      120,
		"m:cached_tokens:qwen3-4b":     80,
		"m:request_count:qwen3-4b":     1,
		"m:completion_tokens:MD-00007": 20,
		"m:prompt_tokens:qwen3-4b":     100,
		"m:completion_tokens:qwen3-4b": 20,
		"m:cached_tokens:MD-00007":     80,
		"m:prompt_tokens:MD-00007":     100,
		"m:request_count:MD-00007":     1,
	} {
		if fields[field] != want {
			t.Errorf("%s = %d, want %d", field, fields[field], want)
		}
	}
}

// A field that never moved should not appear at all — this is the case the control plane's
// billable() clamp exists for, and writing an explicit zero would defeat it.
func TestZeroMetricsAreNotWritten(t *testing.T) {
	fields, _ := UsageFields(Report{
		Model: "qwen3-4b",
		Usage: `{"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10}}`,
	})
	if _, present := fields["completion_tokens"]; present {
		t.Error("a zero completion count was written")
	}
	if _, present := fields["cached_tokens"]; present {
		t.Error("a zero cached count was written — vLLM without --enable-prompt-tokens-details")
	}
}

// m:<metric>:<deployment> shares a namespace with m:<metric>:<model>. When a Model is named the
// same as its Model Deployment, writing both would bill that request twice.
func TestAModelNamedLikeItsDeploymentIsNotDoubleCounted(t *testing.T) {
	fields, _ := UsageFields(Report{Model: "same", Deployment: "same", Usage: openAIUsage})
	if got := fields["m:total_tokens:same"]; got != 120 {
		t.Errorf("m:total_tokens:same = %d, want 120", got)
	}
}

// A request that produced no usage frame still counts as a request. Dropping it would make the
// request count disagree with the access log for every streaming client that hung up early.
func TestARequestWithNoUsageStillCounts(t *testing.T) {
	fields, _ := UsageFields(Report{Model: "qwen3-4b", Usage: ""})
	if fields["request_count"] != 1 {
		t.Errorf("request_count = %d, want 1", fields["request_count"])
	}
	if _, present := fields["total_tokens"]; present {
		t.Error("tokens were invented for a request that reported none")
	}
}

// The prompt is split the way the price list reads it: plain, cached, written for five minutes,
// written for an hour — the seven priced counters beside the display ones, each twinned.
func TestUsageFieldsEmitsThePricedCounters(t *testing.T) {
	anthropic := `{"usage":{"input_tokens":50,"output_tokens":7,"cache_read_input_tokens":30,` +
		`"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":15,"ephemeral_1h_input_tokens":5}}}`
	fields, trusted := UsageFields(Report{Model: "claude", Deployment: "anthropic", Usage: anthropic})
	if !trusted {
		t.Error("a consistent response was flagged")
	}
	for field, want := range map[string]int64{
		"request_count": 1, "prompt_tokens": 100, "completion_tokens": 7, "total_tokens": 107,
		"input_tokens": 50, "cached_tokens": 30, "cache_write_tokens": 15, "cache_write_1h_tokens": 5,
		"m:input_tokens:claude": 50, "m:cache_write_1h_tokens:anthropic": 5,
	} {
		if fields[field] != want {
			t.Errorf("%s = %d, want %d", field, fields[field], want)
		}
	}
	if _, present := fields["audio_seconds"]; present {
		t.Error("audio_seconds written for a chat response")
	}
}

// A duration-shaped transcription reports seconds, not tokens: shown, and nothing priced moves.
func TestUsageFieldsMetersAudioSeconds(t *testing.T) {
	fields, _ := UsageFields(Report{Model: "whisper", Usage: `{"usage":{"type":"duration","seconds":12}}`})
	if fields["audio_seconds"] != 12 || fields["m:audio_seconds:whisper"] != 12 {
		t.Errorf("audio_seconds = %d", fields["audio_seconds"])
	}
	for _, counter := range []string{"input_tokens", "audio_tokens"} {
		if _, present := fields[counter]; present {
			t.Errorf("%s invented for a transcription", counter)
		}
	}
}

// Audio tokens come out of the prompt, so a token is billed once: as audio, cached or plain.
func TestUsageFieldsSplitsAudioOutOfThePrompt(t *testing.T) {
	for name, tc := range map[string]struct {
		usage string
		want  map[string]int64
	}{
		"chat": {
			`{"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,` +
				`"prompt_tokens_details":{"cached_tokens":20,"audio_tokens":30}}}`,
			map[string]int64{"prompt_tokens": 100, "input_tokens": 50, "cached_tokens": 20, "audio_tokens": 30, "m:audio_tokens:m": 30},
		},
		"transcription": {
			`{"usage":{"type":"tokens","input_tokens":14,"output_tokens":45,"total_tokens":59,` +
				`"input_token_details":{"text_tokens":4,"audio_tokens":10}}}`,
			map[string]int64{"input_tokens": 4, "audio_tokens": 10, "completion_tokens": 45},
		},
		"audio beyond what the cache left": {
			`{"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101,` +
				`"prompt_tokens_details":{"cached_tokens":80,"audio_tokens":30}}}`,
			map[string]int64{"input_tokens": 0, "cached_tokens": 80, "audio_tokens": 20},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fields, _ := UsageFields(Report{Model: "m", Usage: tc.usage})
			for field, want := range tc.want {
				if fields[field] != want {
					t.Errorf("%s = %d, want %d", field, fields[field], want)
				}
			}
		})
	}
}

// Cache buckets beyond the prompt cannot be credited: the whole prompt bills as plain, and the
// caller is told so — once per model, since it is a property of the engine, not the request.
func TestCacheBucketsBeyondThePromptBillAsPlain(t *testing.T) {
	overflow := `{"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,` +
		`"prompt_tokens_details":{"cached_tokens":50}}}`
	fields, trusted := UsageFields(Report{Model: "qwen3-4b", Usage: overflow})
	if trusted {
		t.Error("an inconsistent response was trusted")
	}
	if fields["input_tokens"] != 10 {
		t.Errorf("input_tokens = %d, want the whole prompt", fields["input_tokens"])
	}
	if _, present := fields["cached_tokens"]; present {
		t.Error("cached_tokens credited past the prompt")
	}
}

// The store gets the counters, the cost priced off the pricing in force, and the holder's spend in
// one accrual — each priced counter and the cost tagged with the pricing's id, so the pull prices
// them with the same table.
func TestRecordAccruesCostAndSpend(t *testing.T) {
	store := memory.New()
	store.Users["GU-1"] = domain.UserRecord{Prepaid: true, Budget: 10_000_000}
	svc := New(store.Repositories().Usage, store.Repositories().Health, quiet())

	svc.Record(context.Background(), Report{
		Prefix: "abc", Model: "qwen3-4b", Usage: openAIUsage, User: "GU-1", Prepaid: true, Budget: 10_000_000,
		// $3/Mtok plain, $0.30/Mtok cached, $15/Mtok completion, in nano-USD per Mtok.
		Pricing: &domain.Pricing{ID: "mp1", Rates: map[string]int64{"input_tokens": 3e9, "cached_tokens": 3e8, "completion_tokens": 15e9}},
	})
	usage := store.Usage["abc"]
	want := int64(20*3000 + 80*300 + 20*15000)
	for field, n := range map[string]int64{
		"cost": want, "p:mp1:cost": want, "input_tokens": 20, "p:mp1:input_tokens": 20,
		"p:mp1:cached_tokens": 80, "p:mp1:completion_tokens": 20, "p:mp1:request_count": 1,
		"user_spent": want, "user_balance": 10_000_000 - want,
	} {
		if usage[field] != n {
			t.Errorf("usage[%s] = %d, want %d", field, usage[field], n)
		}
	}
	if _, present := usage["m:cost:qwen3-4b"]; present {
		t.Error("m:cost is gone: the pricing id carries the cost now")
	}
	if _, present := usage["p:mp1:prompt_tokens"]; present {
		t.Error("only the seven priced counters are tagged")
	}
	if store.Users["GU-1"].Spent != want {
		t.Errorf("spent = %d, want %d", store.Users["GU-1"].Spent, want)
	}
}

// Without rates nothing costs, but the counters and the request still land, and the holder's
// spend is still reported — a request on an unpriced model is not an invisible one.
func TestAnUnpricedRequestStillAccrues(t *testing.T) {
	store := memory.New()
	store.Users["GU-1"] = domain.UserRecord{Prepaid: true}
	svc := New(store.Repositories().Usage, store.Repositories().Health, quiet())
	svc.Record(context.Background(), Report{Prefix: "abc", Model: "m", Usage: openAIUsage, User: "GU-1", Prepaid: true})

	usage := store.Usage["abc"]
	if _, present := usage["cost"]; present {
		t.Error("a cost was written without rates")
	}
	spent, reported := usage["user_spent"]
	if usage["total_tokens"] != 120 || !reported || spent != 0 {
		t.Errorf("usage = %v", usage)
	}
}

// A free holder's usage is counted and priced like anyone's, but their spend never moves and the
// drain carries no balance for them: there is nothing to gate and nothing owed.
func TestAFreeHoldersSpendNeverMoves(t *testing.T) {
	store := memory.New()
	store.Users["GU-1"] = domain.UserRecord{Spent: 40}
	svc := New(store.Repositories().Usage, store.Repositories().Health, quiet())
	svc.Record(context.Background(), Report{
		Prefix: "abc", Model: "m", Usage: openAIUsage, User: "GU-1",
		Pricing: &domain.Pricing{ID: "mp1", Rates: map[string]int64{"completion_tokens": 15e9}},
	})

	usage := store.Usage["abc"]
	if usage["p:mp1:cost"] != 20*15000 || usage["p:mp1:completion_tokens"] != 20 {
		t.Errorf("a free holder's usage is still priced: usage = %v", usage)
	}
	for _, field := range []string{"user_spent", "user_balance"} {
		if _, present := usage[field]; present {
			t.Errorf("%s reported for a free holder", field)
		}
	}
	if store.Users["GU-1"].Spent != 40 {
		t.Errorf("spent = %d, want it left at 40", store.Users["GU-1"].Spent)
	}
}

// The usage frame arrives SSE-wrapped on a streaming response and bare on a non-streaming one.
// Both have to parse, or every streaming request bills as zero.
func TestAStreamingFrameParses(t *testing.T) {
	streamed, _ := UsageFields(Report{Model: "qwen3-4b", Usage: "data: " + openAIUsage})
	if streamed["total_tokens"] != 120 {
		t.Errorf("total_tokens = %d from an SSE frame, want 120", streamed["total_tokens"])
	}
}

// A report with no prefix has no bucket to accrue to, but how its target behaved is not contingent
// on that — a broken engine must still be counted as broken.
func TestOutcomeIsRecordedWithoutAPrefix(t *testing.T) {
	store := memory.New()
	New(store.Repositories().Usage, store.Repositories().Health, quiet()).Record(
		context.Background(),
		Report{Target: "https://a", UpstreamStatus: "502"},
	)
	if store.Failures["https://a"] != 1 {
		t.Errorf("failures = %d, want 1", store.Failures["https://a"])
	}
	if len(store.Usage) != 0 {
		t.Errorf("usage accrued to %v with no prefix", store.Usage)
	}
}

// A success clears the count outright, so a target has to fail EjectAfter times in a ROW.
func TestOneSuccessClearsTheFailureStreak(t *testing.T) {
	store := memory.New()
	store.Failures["https://a"] = domain.EjectAfter - 1
	svc := New(store.Repositories().Usage, store.Repositories().Health, quiet())

	svc.Record(context.Background(), Report{Prefix: "abc", Target: "https://a", UpstreamStatus: "200"})
	if store.Failures["https://a"] != 0 {
		t.Errorf("failures = %d after a success, want 0", store.Failures["https://a"])
	}
}

// One unplaced model must not take an ingress out of rotation for every other model on it.
func TestANoReplica503DoesNotCountAgainstTheTarget(t *testing.T) {
	store := memory.New()
	New(store.Repositories().Usage, store.Repositories().Health, quiet()).Record(
		context.Background(),
		Report{Target: "https://ingress", UpstreamStatus: "503", Reason: "no-replica"},
	)
	if store.Failures["https://ingress"] != 0 {
		t.Errorf("failures = %d, want 0 — the ingress answered correctly", store.Failures["https://ingress"])
	}
}

// The first usage line carries what the last one left out; one with nothing to read is ignored.
func TestUsageFieldsMergesTheFirstUsageLine(t *testing.T) {
	start := `data: {"type":"message_start","message":{"usage":{"input_tokens":66,"cache_read_input_tokens":2,` +
		`"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":3},"output_tokens":1}}}`
	last := `data: {"type":"message_delta","usage":{"output_tokens":26}}`
	fields, _ := UsageFields(Report{Model: "claude", UsageStart: start, Usage: last})
	for field, want := range map[string]int64{
		"prompt_tokens": 73, "completion_tokens": 26, "total_tokens": 99, "input_tokens": 66,
		"cached_tokens": 2, "cache_write_tokens": 2, "cache_write_1h_tokens": 3,
	} {
		if fields[field] != want {
			t.Errorf("%s = %d, want %d", field, fields[field], want)
		}
	}

	null := `data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}`
	fields, _ = UsageFields(Report{Model: "gpt", UsageStart: null, Usage: "data: " + openAIUsage})
	if fields["prompt_tokens"] != 100 || fields["total_tokens"] != 120 {
		t.Errorf("a null first line moved the counts: %v", fields)
	}
}
