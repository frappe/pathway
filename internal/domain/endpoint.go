package domain

import "strings"

var endpointModalities = map[string]map[string]bool{
	"/v1/chat/completions":     {"text": true, "multimodal": true},
	"/v1/messages":             {"text": true, "multimodal": true},
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

// ServesRoute reports whether this route answers on this surface and path — the model's modality
// when it is an engine of ours, and the pushed dialect when it is a vendor. A provider is a closed
// set where an engine is not: anything but its dialect's own paths is a 404 from it, after a
// round trip we paid for — so it is our 404 instead.
func ServesRoute(r Route, dialect, path string) bool {
	if r.IsProvider() {
		return PathDialect(path) == dialect && r.SpeaksDialect(dialect)
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
