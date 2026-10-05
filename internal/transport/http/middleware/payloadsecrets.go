package middleware

import (
	"regexp"
	"strconv"
	"strings"
)

// secretFormat is one detectable secret. Go's regexp has no fast path for these shapes — run over
// every body it costs about 25ms a megabyte each — so each is guarded by a literal it cannot match
// without, and a substring search is a hundred times cheaper.
type secretFormat struct {
	label   string
	anchors []string
	re      *regexp.Regexp
}

// possible is the guard: false means this format cannot be in s, and its regexp is skipped.
// ponytail: a guard that passes runs the regexp over the whole text — ~185ms a megabyte when every
// anchor is present (0x, eyJ, dp. all common in code); run it on a window around each anchor hit if
// that ever shows on a box.
func (f secretFormat) possible(s string) bool {
	for _, anchor := range f.anchors {
		if strings.Contains(s, anchor) {
			return true
		}
	}
	return false
}

// secretFormats are the formats OpenRouter's Secrets guardrail detects
// (https://openrouter.ai/docs/guides/features/guardrails/secret-formats), under its labels. A
// known prefix is enough, which is OpenRouter's own rule for most rows ("a prefix followed by a
// token body"): at least secretTail token characters after it, of any length past that, go. Where
// OpenRouter pins a length or a marker (OpenAI's T3BlbkFJ, Anthropic's trailing AA, 64 hex) this is
// a superset of its match, so a key a vendor lengthened or one of an odd size is still caught. The
// floor is what keeps a name in code (hf_token, ghp_example) from reading as a key. Two keep a
// strict shape because their prefix says too little alone: an Ethereum key (0x is every address and
// hash in code) and a JWT (three dotted parts). OpenRouter's two Bitcoin WIF formats are left out:
// they have no prefix, only a length and an alphabet ordinary ids share.
// Order matters only where two could claim the same text, and then the specific one is first.
var secretFormats = []secretFormat{
	// Our own: a Grove key (gr_ and an alphanumeric body) pasted into a prompt is as much a leak as
	// any vendor's.
	{label: "grove-api-key", anchors: []string{"gr_"}, re: tail(`gr_`, `[A-Za-z0-9]`)},
	{label: "private-key-block", anchors: []string{"PRIVATE KEY"}, re: regexp.MustCompile(`-----BEGIN[A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----(?:[\s\S]*?-----END[A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|[\s\S]*$)`)},
	{label: "aws-access-key-id", anchors: []string{"AKIA", "ASIA", "ABIA", "ACCA", "A3T"}, re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA|A3T[A-Z0-9])[A-Z0-9]{12,}\b`)},
	{label: "github-fine-grained-pat", anchors: []string{"github_pat_"}, re: tail(`github_pat_`, `[A-Za-z0-9_]`)},
	{label: "github-token", anchors: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, re: tail(`gh[pousr]_`, `[A-Za-z0-9]`)},
	{label: "gitlab-personal-access-token", anchors: []string{"glpat-"}, re: tail(`glpat-`, `[A-Za-z0-9_-]`)},
	{label: "anthropic-api-key", anchors: []string{"sk-ant-"}, re: tail(`sk-ant-`, `[A-Za-z0-9_-]`)},
	{label: "openrouter-api-key", anchors: []string{"sk-or-v1-"}, re: tail(`sk-or-v1-`, `[A-Za-z0-9]`)},
	{label: "openai-api-key", anchors: []string{"sk-proj-", "sk-svcacct-", "sk-admin-"}, re: tail(`sk-(?:proj|svcacct|admin)-`, `[A-Za-z0-9_-]`)},
	// After every sk-<vendor>- form: a bare sk- with a long alphanumeric run is OpenAI's old key.
	{label: "openai-legacy-api-key", anchors: []string{"sk-"}, re: regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}`)},
	{label: "google-api-key", anchors: []string{"AIza"}, re: tail(`AIza`, `[0-9A-Za-z_-]`)},
	{label: "google-oauth-client-secret", anchors: []string{"GOCSPX-"}, re: tail(`GOCSPX-`, `[A-Za-z0-9_-]`)},
	{label: "stripe-secret-key", anchors: []string{"k_live_", "k_test_"}, re: tail(`[sr]k_(?:live|test)_`, `[A-Za-z0-9]`)},
	{label: "slack-token", anchors: []string{"xox"}, re: tail(`xox[bpos]-`, `[A-Za-z0-9-]`)},
	{label: "slack-legacy-workspace-token", anchors: []string{"xox"}, re: tail(`xox[ar]-`, `[A-Za-z0-9-]`)},
	{label: "slack-app-token", anchors: []string{"xapp-"}, re: tail(`xapp-`, `[A-Za-z0-9-]`)},
	{label: "slack-webhook-url", anchors: []string{"hooks.slack.com/"}, re: regexp.MustCompile(`https://hooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9_/]+`)},
	{label: "npm-access-token", anchors: []string{"npm_"}, re: tail(`npm_`, `[A-Za-z0-9]`)},
	// SG. alone is too common, so its two dotted parts stay; their lengths do not.
	{label: "sendgrid-api-key", anchors: []string{"SG."}, re: regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`)},
	{label: "huggingface-access-token", anchors: []string{"hf_"}, re: tail(`hf_`, `[A-Za-z0-9]`)},
	{label: "databricks-api-token", anchors: []string{"dapi"}, re: tail(`dapi`, `[a-f0-9]`)},
	{label: "atlassian-api-token", anchors: []string{"ATATT3"}, re: tail(`ATATT3`, `[A-Za-z0-9_=-]`)},
	{label: "doppler-token", anchors: []string{"dp."}, re: tail(`dp\.(?:pt|st|ct|sa|scim|audit)\.`, `[A-Za-z0-9._-]`)},
	{label: "linear-api-key", anchors: []string{"lin_api_"}, re: tail(`lin_api_`, `[A-Za-z0-9]`)},
	{label: "shopify-access-token", anchors: []string{"shp"}, re: tail(`shp(?:at|ca|pa|ss)_`, `[a-fA-F0-9]`)},
	{label: "telegram-bot-token", anchors: []string{":AA"}, re: tail(`[0-9]{8,10}:AA`, `[A-Za-z0-9_-]`)},
	{label: "age-secret-key", anchors: []string{"AGE-SECRET-KEY-1"}, re: tail(`AGE-SECRET-KEY-1`, `[A-Z0-9]`)},
	{label: "json-web-token", anchors: []string{"eyJ"}, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)},
	{label: "bitcoin-extended-private-key", anchors: []string{"prv"}, re: tail(`[xyzYZtuvUV]prv`, `[1-9A-HJ-NP-Za-km-z]`)},
	// The shape of a transaction hash too; OpenRouter accepts that cost, and so does this.
	{label: "ethereum-private-key", anchors: []string{"0x"}, re: regexp.MustCompile(`\b0x[0-9a-fA-F]{64}\b`)},
	{label: "pypi-upload-token", anchors: []string{"pypi-AgEIcHlwaS5vcmc"}, re: tail(`pypi-AgEIcHlwaS5vcmc`, `[A-Za-z0-9_-]`)},
	{label: "digitalocean-token", anchors: []string{"_v1_"}, re: tail(`do[pors]_v1_`, `[a-f0-9]`)},
}

// secretTail is the fewest characters after a prefix that make it a key rather than a name.
const secretTail = 16

// tail is a prefix followed by at least secretTail characters of class, and then any more of them.
func tail(prefix, class string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + prefix + class + `{` + strconv.Itoa(secretTail) + `,}`)
}

// mayHoldSecret is the cheap gate: a body nothing matches is logged untouched, without decoding.
func mayHoldSecret(body []byte) bool {
	s := string(body)
	for _, format := range secretFormats {
		if format.possible(s) && format.re.MatchString(s) {
			return true
		}
	}
	return false
}

// redactSecrets replaces every secret in one piece of text with [SECRET:<label>], in the list's
// order, so a specific format claims its text before a general one sees it.
func redactSecrets(s string) (string, bool) {
	changed := false
	for _, format := range secretFormats {
		if !format.possible(s) || !format.re.MatchString(s) {
			continue
		}
		s = format.re.ReplaceAllLiteralString(s, "[SECRET:"+format.label+"]")
		changed = true
	}
	return s, changed
}
