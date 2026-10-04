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

// scrubPayload readies a body for the payload log: inline media over mediaKeep becomes a
// placeholder and secrets become [SECRET:<label>]. Secrets are looked for in each JSON string that
// is not media — a base64 image can hold runs shaped like a key, and redacting
// those would corrupt an image the log is keeping. A body with nothing to change comes back as it
// was, byte-for-byte; most never get decoded.
func scrubPayload(body []byte) []byte {
	if len(body) <= mediaKeep && !mayHoldSecret(body) {
		return body
	}
	var doc any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber() // a re-encoded number must read as the client wrote it
	if decoder.Decode(&doc) != nil {
		// Not JSON: text, scanned whole. It holds no media this could recognise.
		if redacted, changed := redactSecrets(string(body)); changed {
			return []byte(redacted)
		}
		return body
	}
	if !scrubValue(doc) {
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

// scrubStream is scrubPayload per `data:` line of an event stream. A secret the model streams
// across several events is never whole in one, and is not caught.
func scrubStream(body []byte) []byte {
	if len(body) <= mediaKeep && !mayHoldSecret(body) {
		return body
	}
	lines := bytes.Split(body, []byte("\n"))
	for i, line := range lines {
		if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			payload = bytes.TrimSpace(payload)
			if scrubbed := scrubPayload(payload); !bytes.Equal(scrubbed, payload) {
				lines[i] = append([]byte("data: "), scrubbed...)
			}
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// scrubValue walks a decoded document in place and reports whether it changed anything.
func scrubValue(v any) bool {
	changed := false
	switch node := v.(type) {
	case map[string]any:
		for key, child := range node {
			if s, ok := child.(string); ok {
				if scrubbed, did := scrubString(key, s, node); did {
					node[key], changed = scrubbed, true
				}
				continue
			}
			changed = scrubValue(child) || changed
		}
	case []any:
		for i, child := range node {
			if s, ok := child.(string); ok {
				if scrubbed, did := scrubString("", s, nil); did {
					node[i], changed = scrubbed, true
				}
				continue
			}
			changed = scrubValue(child) || changed
		}
	}
	return changed
}

// scrubString is one string value: media is replaced when big and otherwise kept untouched;
// anything else is text, and has its secrets redacted.
func scrubString(key, s string, parent map[string]any) (string, bool) {
	mediaType, encoded, isMedia := mediaOf(key, s, parent)
	if !isMedia {
		return redactSecrets(s)
	}
	if len(s) <= mediaKeep {
		return s, false
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

// mediaMin is the shortest base64 under a media key taken for media. Below it a value is checked
// as text — an access key someone put in a `data` field is short and must still be caught.
const mediaMin = 1 << 10

// mediaOf says whether a string is inline media: a data: URI anywhere, or base64 under a media
// key, with its type from the URI or a sibling `media_type` / `format`.
func mediaOf(key, s string, parent map[string]any) (mediaType, encoded string, ok bool) {
	if strings.HasPrefix(s, "data:") && strings.Contains(s, ";base64,") {
		mediaType, encoded, _ = strings.Cut(strings.TrimPrefix(s, "data:"), ";base64,")
		return mediaType, encoded, true
	}
	if !mediaKeys[key] || len(s) < mediaMin || !looksBase64(s) {
		return "", "", false
	}
	mediaType = "application/octet-stream"
	for _, sibling := range []string{"media_type", "format"} {
		if t, ok := parent[sibling].(string); ok && t != "" {
			mediaType = t
			break
		}
	}
	return mediaType, s, true
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
