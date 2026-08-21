package domain

import "strings"

// Which model answers on which OpenAI surface. An ASR model and a chat model are both just a name
// in the route table, so without this a transcription request routes happily to a chat engine, gets
// a 404 from it, and the caller is billed a request for the round trip.

// endpointModalities names, per path, the modalities that answer on it. A path no entry claims is
// forwarded whatever the model is: engines serve more than the OpenAI core — /tokenize, /v1/rerank,
// /v1/score — and refusing those here would take away endpoints that work today.
var endpointModalities = map[string]map[string]bool{
	"/v1/chat/completions": {"text": true, "multimodal": true},
	// The Anthropic-shaped twin of chat/completions. Claimed for the same modalities, because the
	// two spellings are one request and enforcing only one of them is an accident, not a policy.
	"/v1/messages":             {"text": true, "multimodal": true},
	"/v1/completions":          {"text": true, "multimodal": true},
	"/v1/embeddings":           {"embedding": true},
	"/v1/audio/transcriptions": {"audio": true},
	"/v1/audio/translations":   {"audio": true},
}

// knownModalities is what this build understands. A value outside it comes from a control plane
// newer than this binary, and is treated as unrestricted — a fleet mid-upgrade must not start
// refusing traffic because one side learned a word first.
var knownModalities = map[string]bool{
	"text": true, "multimodal": true, "embedding": true, "audio": true,
}

// anthropicSurfaces is everything the one vendor dialect we speak answers on. A provider is a
// closed set where an engine is not, so this is an allowlist where endpointModalities is a
// denylist: a path a vendor does not serve is a 404 from them, after a round trip we paid for.
// ponytail: one dialect. A second means keying this on Route.Provider, or pushing the set.
var anthropicSurfaces = map[string]bool{
	"/v1/messages":              true,
	"/v1/messages/count_tokens": true,
}

// ServesRoute reports whether this route answers on this path — the vendor's own list when it is
// one, and the model's modality when it is an engine of ours.
func ServesRoute(r Route, path string) bool {
	if r.IsProvider() {
		return anthropicSurfaces[strings.TrimRight(path, "/")]
	}
	return Serves(r.Modality, path)
}

// Serves reports whether a model of this modality answers on this path. Pure, and deliberately
// generous: it refuses only when the path is claimed by a modality this model does not have.
func Serves(modality, path string) bool {
	modality = strings.TrimSpace(modality)
	if modality == "" || !knownModalities[modality] {
		return true
	}
	allowed, claimed := endpointModalities[strings.TrimRight(path, "/")]
	if !claimed {
		return true
	}
	return allowed[modality]
}
