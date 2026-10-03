package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// mediaKeep is the largest inline media item the payload log keeps as sent. A screenshot fits; a
// photo or a minute of audio does not, and is logged as a placeholder naming its type, size and
// hash instead — enough for support to match a file the customer sends, without keeping it. Text
// is never cut (user call, 2026-08-31); only media past this is replaced.
const mediaKeep = 256 << 10

// mediaKeys hold raw base64 media on either surface: OpenAI's input_audio.data, audio.data and
// data[].b64_json, Anthropic's source.data.
var mediaKeys = map[string]bool{"data": true, "b64_json": true}

// redactMedia replaces every inline media item over mediaKeep in a JSON document. A body that is
// not JSON, or has nothing to replace, comes back as it was — byte-for-byte.
func redactMedia(body []byte) []byte {
	if len(body) <= mediaKeep {
		return body
	}
	var doc any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber() // a re-encoded number must read as the client wrote it
	if decoder.Decode(&doc) != nil || !replaceMedia(doc) {
		return body
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false) // a prompt's < and & stay as typed
	if encoder.Encode(doc) != nil {
		return body
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// redactStreamMedia is redactMedia per `data:` line of an event stream.
func redactStreamMedia(body []byte) []byte {
	if len(body) <= mediaKeep {
		return body
	}
	lines := bytes.Split(body, []byte("\n"))
	for i, line := range lines {
		if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok && len(payload) > mediaKeep {
			if redacted := redactMedia(bytes.TrimSpace(payload)); !bytes.Equal(redacted, bytes.TrimSpace(payload)) {
				lines[i] = append([]byte("data: "), redacted...)
			}
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// replaceMedia walks a decoded document in place and reports whether it replaced anything.
func replaceMedia(v any) bool {
	changed := false
	switch node := v.(type) {
	case map[string]any:
		for key, child := range node {
			if s, ok := child.(string); ok {
				if stand, ok := mediaPlaceholder(key, s, node); ok {
					node[key], changed = stand, true
				}
				continue
			}
			changed = replaceMedia(child) || changed
		}
	case []any:
		for i, child := range node {
			if s, ok := child.(string); ok {
				if stand, ok := mediaPlaceholder("", s, nil); ok {
					node[i], changed = stand, true
				}
				continue
			}
			changed = replaceMedia(child) || changed
		}
	}
	return changed
}

// mediaPlaceholder names a media string over mediaKeep: a data: URI anywhere, or base64 under a
// media key. Anything else — long text included — is not media and is kept.
func mediaPlaceholder(key, s string, parent map[string]any) (string, bool) {
	if len(s) <= mediaKeep {
		return "", false
	}
	var mediaType, encoded string
	switch {
	case strings.HasPrefix(s, "data:") && strings.Contains(s, ";base64,"):
		mediaType, encoded, _ = strings.Cut(strings.TrimPrefix(s, "data:"), ";base64,")
	case mediaKeys[key] && looksBase64(s):
		encoded = s
		mediaType = "application/octet-stream"
		for _, sibling := range []string{"media_type", "format"} {
			if t, ok := parent[sibling].(string); ok && t != "" {
				mediaType = t
				break
			}
		}
	default:
		return "", false
	}
	// Hashed and sized as the file it is, so it matches the customer's copy. Undecodable is still
	// replaced — it is not text anyone reads — and then hashed as sent.
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw = []byte(encoded)
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("[media %s %d bytes sha256:%s]", mediaType, len(raw), hex.EncodeToString(sum[:8])), true
}

// looksBase64 checks the head only: a long `data` field of prose is not media and must be kept.
func looksBase64(s string) bool {
	for _, c := range s[:min(len(s), 1024)] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
			return false
		}
	}
	return true
}

// binaryMediaType reports whether a response is a file rather than text — speech audio, a
// rendered image — which a JSON log line cannot hold as it is.
func binaryMediaType(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.TrimSpace(strings.ToLower(mediaType))
	for _, prefix := range []string{"audio/", "image/", "video/"} {
		if strings.HasPrefix(mediaType, prefix) {
			return true
		}
	}
	return mediaType == "application/octet-stream"
}
