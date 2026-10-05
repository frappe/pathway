package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r is a run of one character, so the samples below are assembled rather than written out whole —
// a repo secret scanner reading this file must not take a test fixture for a leaked key.
func r(s string, n int) string { return strings.Repeat(s, n) }

// One sample per format OpenRouter lists, each under its label.
func TestEverySecretFormatIsRedacted(t *testing.T) {
	for label, secret := range map[string]string{
		"grove-api-key":                "gr_" + r("k", 32),
		"private-key-block":            "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIabc\n-----END RSA " + "PRIVATE KEY-----",
		"aws-access-key-id":            "AKIA" + "IOSFODNN7EXAMPLE",
		"github-fine-grained-pat":      "github_pat_" + r("A", 22) + "_" + r("b", 59),
		"github-token":                 "ghp_" + r("a", 36),
		"gitlab-personal-access-token": "glpat-" + r("x", 20),
		"openai-api-key":               "sk-proj-" + r("x", 40),
		"openai-legacy-api-key":        "sk-" + r("a", 20) + "T3BlbkFJ" + r("b", 20),
		"anthropic-api-key":            "sk-ant-api03-" + r("x", 90) + "AA",
		"openrouter-api-key":           "sk-or-v1-" + r("a", 64),
		"google-api-key":               "AIza" + r("b", 35),
		"google-oauth-client-secret":   "GOCSPX-" + r("c", 28),
		"stripe-secret-key":            "sk_live_" + r("d", 24),
		"slack-token":                  "xoxb-" + "123456789012-" + "abcDEF",
		"slack-legacy-workspace-token": "xoxa-" + "2-" + r("j", 16),
		"slack-app-token":              "xapp-" + "1-A0123-1234-abcdef",
		"slack-webhook-url":            "https://hooks.slack.com/services/" + "T000/B000/XXXX",
		"npm-access-token":             "npm_" + r("e", 36),
		"sendgrid-api-key":             "SG." + r("f", 22) + "." + r("g", 43),
		"huggingface-access-token":     "hf_" + r("h", 34),
		"databricks-api-token":         "dapi" + r("a", 32),
		"atlassian-api-token":          "ATATT3" + r("x", 190),
		"doppler-token":                "dp.pt." + r("z", 43),
		"linear-api-key":               "lin_api_" + r("q", 40),
		"shopify-access-token":         "shpat_" + r("a", 32),
		"telegram-bot-token":           "123456789:AA" + r("x", 33),
		"age-secret-key":               "AGE-SECRET-KEY-1" + r("Q", 58),
		"json-web-token":               "eyJ" + "hbGciOiJIUzI1NiJ9" + ".eyJ" + "zdWIiOiIxMjM0In0" + "." + r("s", 16),
		"bitcoin-extended-private-key": "xprv" + r("9", 108),
		"ethereum-private-key":         "0x" + r("a", 64),
		"pypi-upload-token":            "pypi-AgEIcHlwaS5vcmc" + r("x", 60),
		"digitalocean-token":           "dop_v1_" + r("a", 64),
	} {
		got, changed := redactSecrets("my key is " + secret + " ok")
		if want := "my key is [SECRET:" + label + "] ok"; !changed || got != want {
			t.Errorf("%s: got %q, want %q", label, got, want)
		}
	}
	// OpenRouter lists 33; its two Bitcoin WIF formats are left out on purpose, and Grove's own added.
	if len(secretFormats) != 32 {
		t.Errorf("%d formats, want OpenRouter's 33 less the two WIF plus Grove's", len(secretFormats))
	}
}

// A known prefix is enough: a key of a length no format documents is still caught, and so is an
// OpenAI key without the marker older ones carried, and a private key cut off before its end line.
func TestAPrefixIsEnoughWhateverTheLength(t *testing.T) {
	for label, secret := range map[string]string{
		"aws-access-key-id":        "AKIA" + r("Z", 20),
		"github-token":             "ghp_" + r("a", 20),
		"openrouter-api-key":       "sk-or-v1-" + r("a", 63),
		"openai-api-key":           "sk-proj-" + r("Q", 120),
		"anthropic-api-key":        "sk-ant-api03-" + r("x", 30),
		"huggingface-access-token": "hf_" + r("h", 50),
		"private-key-block":        "-----BEGIN " + "PRIVATE KEY-----\nMIIcutoff",
		"slack-webhook-url":        "https://hooks.slack.com/" + "workflows/T000/A000/XXXX",
	} {
		got, _ := redactSecrets("key " + secret)
		if want := "key [SECRET:" + label + "]"; got != want {
			t.Errorf("%s: got %q, want %q", label, got, want)
		}
	}
}

// Below the floor a prefix is a name, not a key — and the three strict shapes stay strict.
func TestNamesAndNearMissesAreKept(t *testing.T) {
	for _, text := range []string{
		"hf_token", "ghp_example", "gr_demo", "sk-learn", "sk-or-v1-short", "npm_install", "AKIA" + r("A", 11),
		"0x" + r("a", 63), // an Ethereum key is exactly 64 hex
		"K" + r("z", 51),  // WIF-shaped: not detected, by choice
		"eyJhbGciOi only one part",
		"ask-the-team about sk- prefixes and the eyJ header",
		"dapi is a word",
	} {
		if got, changed := redactSecrets(text); changed {
			t.Errorf("%q was redacted to %q", text, got)
		}
	}
}

// End to end through the log: prompt, stream output and upload fields, a key in a kept image left
// alone, and a body with nothing in it logged byte-for-byte.
func TestThePayloadLineCarriesNoSecrets(t *testing.T) {
	ghp := "ghp_" + r("a", 36)
	aws := "AKIA" + "IOSFODNN7EXAMPLE"
	pem := `-----BEGIN ` + `PRIVATE KEY-----\nMIIsecretbody\n-----END ` + `PRIVATE KEY-----`
	// A base64 image with a run shaped like an AWS key, between two '+'.
	image := "data:image/png;base64," + r("A", 600) + "+AKIA" + r("Z", 16) + "+" + r("A", 600)

	t.Run("prompt", func(t *testing.T) {
		body := `{"messages":[{"role":"user","content":"deploy with ` + ghp + `"},{"role":"user","content":"` + pem + `"},` +
			`{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + image + `"}}]}],"n":1.50}`
		line, _ := payloadLine(t, func(s *State) { s.Raw = []byte(body) }, post(),
			func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
		prompt := line["prompt"].(string)
		var doc map[string]any
		if err := json.Unmarshal([]byte(prompt), &doc); err != nil {
			t.Fatalf("redacted prompt is not JSON: %v", err)
		}
		for _, want := range []string{"[SECRET:github-token]", "[SECRET:private-key-block]", image, `"n":1.50`} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt lacks %.60q", want)
			}
		}
		for _, leaked := range []string{ghp, "MIIsecretbody"} {
			if strings.Contains(prompt, leaked) {
				t.Errorf("prompt still holds %q", leaked)
			}
		}
	})

	t.Run("stream", func(t *testing.T) {
		frame := `data: {"choices":[{"delta":{"content":"here: ` + aws + `"}}]}` + "\n\n"
		line, w := payloadLine(t, func(s *State) { s.Raw = []byte(`{}`) }, post(),
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, frame)
			})
		if out := line["output"].(string); strings.Contains(out, aws) || !strings.Contains(out, "[SECRET:aws-access-key-id]") {
			t.Errorf("output = %q", out)
		}
		if w.Body.String() != frame {
			t.Error("the client's stream was changed")
		}
	})

	t.Run("upload", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
		line, _ := payloadLine(t, func(s *State) { s.Form = map[string]string{"model": "whisper", "prompt": "token " + ghp} }, req,
			func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"text":"hi"}`) })
		if prompt := line["prompt"].(string); strings.Contains(prompt, ghp) {
			t.Errorf("upload prompt = %q", prompt)
		}
	})

	t.Run("nothing to redact", func(t *testing.T) {
		body := `{"model": "qwen",   "messages":[{"content":"a < b & c"}]}`
		line, _ := payloadLine(t, func(s *State) { s.Raw = []byte(body) }, post(),
			func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
		if line["prompt"] != body {
			t.Errorf("prompt = %q, want the body byte-for-byte", line["prompt"])
		}
	})
}
