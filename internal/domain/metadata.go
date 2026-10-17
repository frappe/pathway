package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Limits on X-Grove-Metadata: it lands on every access line, so a caller cannot grow that line
// without bound.
const (
	MetadataMaxBytes = 2048
	MetadataMaxPairs = 16
	MetadataMaxValue = 128
)

var metadataKey = regexp.MustCompile(`^[a-z0-9_.-]{1,32}$`)

// ParseMetadata reads X-Grove-Metadata, `app=hrms, trace=7f3a`, into the caller's own tags for the
// access line. Blank is nil; a repeated key keeps its last value.
func ParseMetadata(header string) (map[string]string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, nil
	}
	if len(header) > MetadataMaxBytes {
		return nil, fmt.Errorf("longer than %d bytes", MetadataMaxBytes)
	}
	pairs := strings.Split(header, ",")
	if len(pairs) > MetadataMaxPairs {
		return nil, fmt.Errorf("more than %d pairs", MetadataMaxPairs)
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, found := strings.Cut(pair, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !found {
			return nil, fmt.Errorf("%q is not key=value", strings.TrimSpace(pair))
		}
		if !metadataKey.MatchString(key) {
			return nil, fmt.Errorf("key %q must be 1-32 of a-z 0-9 _ . -", key)
		}
		if err := checkMetadataValue(value); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		out[key] = value
	}
	return out, nil
}

func checkMetadataValue(value string) error {
	if value == "" || len(value) > MetadataMaxValue {
		return fmt.Errorf("value must be 1-%d characters", MetadataMaxValue)
	}
	for _, c := range value {
		if c < 0x20 || c > 0x7e {
			return errors.New("value must be printable ASCII")
		}
	}
	return nil
}
