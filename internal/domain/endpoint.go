package domain

import (
	"encoding/json"
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

// partInputs is the input a content part asks of a model, by the part's `type` on either surface.
// Text is not here: every model takes it. Images and files are the parts looked for yet; any other
// is the upstream's to refuse.
// ponytail: every Anthropic document is a file, one of plain text too; read its source.type if
// that skips fallbacks that would have served.
var partInputs = map[string]string{
	"image_url": "image",
	"image":     "image",
	"file":      "file",
	"document":  "file",
}

// SentInputs is what a chat or messages body carries that partInputs knows of, in the words a
// model's inputs are declared in. A body it cannot read sends nothing it knows of: that one is the
// upstream's to refuse.
// ponytail: copies each message's content once; a token scan if big bodies fall back often.
func SentInputs(body []byte) []string {
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &request)
	var sent []string
	for _, message := range request.Messages {
		sent = partsInputs(message.Content, sent)
	}
	return sent
}

// partsInputs adds what one content list carries, with the lists inside it: a tool result's own,
// and the parts an Anthropic document is built of. A tool call's input is the caller's JSON, not a
// part, and is not looked into.
func partsInputs(content json.RawMessage, sent []string) []string {
	var parts []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Source  struct {
			Content json.RawMessage `json:"content"`
		} `json:"source"`
	}
	// The error is not read: a string is text and decodes to no parts, and a part with a field of
	// the wrong shape must not hide the ones beside it.
	_ = json.Unmarshal(content, &parts)
	for _, part := range parts {
		if input, known := partInputs[part.Type]; known && !slices.Contains(sent, input) {
			sent = append(sent, input)
		}
		sent = partsInputs(part.Content, sent)
		sent = partsInputs(part.Source.Content, sent)
	}
	return sent
}

// Takes reports whether a model taking `inputs` can be sent a request carrying `sent`. As generous
// as Serves: a model that declares nothing is unrestricted.
func Takes(inputs, sent []string) bool {
	if len(inputs) == 0 {
		return true
	}
	for _, input := range sent {
		if !slices.Contains(inputs, input) {
			return false
		}
	}
	return true
}

// SentTools is every name a body asks a tool by, each once: the `type` of each entry of `tools`,
// and each top-level field, since `web_search_options` or `mcp_servers` is how a shape asks for
// a vendor-run tool with no `tools` entry. Read by exact key off the object, not a struct: a
// struct decode matches `Type` for `type`, and a vendor does not. A `tools` that is not a list,
// or an entry that is not an object or has no string type, names nothing: the upstream's to refuse.
func SentTools(body []byte) []string {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	var sent []string
	for field := range fields {
		sent = append(sent, field)
	}
	slices.Sort(sent)

	var tools []map[string]json.RawMessage
	_ = json.Unmarshal(fields["tools"], &tools)
	for _, tool := range tools {
		var kind string
		if json.Unmarshal(tool["type"], &kind) == nil && kind != "" && !slices.Contains(sent, kind) {
			sent = append(sent, kind)
		}
	}
	return sent
}

// Refuses is the first name in `sent` that a route denying `denied` would not run, "" when none.
// A route that denies nothing runs everything, which is every row we run ourselves.
func Refuses(denied, sent []string) string {
	for _, name := range sent {
		if slices.Contains(denied, name) {
			return name
		}
	}
	return ""
}
