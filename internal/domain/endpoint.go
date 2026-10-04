package domain

import (
	"slices"
	"strings"
)

// endpointOutputs is the output a model must give to answer on a path. Only the paths one kind of
// model serves are here; any other is nobody's to refuse.
var endpointOutputs = map[string]string{
	"/v1/chat/completions":     "text",
	"/v1/messages":             "text",
	"/v1/embeddings":           "embeddings",
	"/v1/audio/transcriptions": "transcription",
	"/v1/audio/translations":   "transcription",
}

// ServesRoute reports whether this route answers on this surface and path — what the model gives
// when it is an engine of ours, and the pushed dialect when it is a vendor. A provider is a closed
// set where an engine is not: anything but its dialect's own paths is a 404 from it, after a
// round trip we paid for — so it is our 404 instead.
func ServesRoute(r Route, dialect, path string) bool {
	if r.IsProvider() {
		return PathDialect(path) == dialect && r.SpeaksDialect(dialect)
	}
	return Serves(r.OutputModalities, path)
}

// Serves reports whether a model giving `outputs` answers on this path. Pure, and deliberately
// generous: it refuses only a path claimed by an output the model does not give. A model that
// declares nothing — a row from before the control plane said — is unrestricted.
func Serves(outputs []string, path string) bool {
	need, claimed := endpointOutputs[strings.TrimRight(path, "/")]
	return !claimed || len(outputs) == 0 || slices.Contains(outputs, need)
}
