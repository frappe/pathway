package domain

import (
	"maps"
	"strings"
	"testing"
)

func TestParseMetadata(t *testing.T) {
	for _, c := range []struct {
		name, header string
		want         map[string]string
		fails        bool
	}{
		{"absent", "", nil, false},
		{"pairs, spaced", " app=hrms ,trace = 7f3a-91", map[string]string{"app": "hrms", "trace": "7f3a-91"}, false},
		{"last of a repeated key wins", "app=a,app=b", map[string]string{"app": "b"}, false},
		{"value with spaces and =", "note=a b=c", map[string]string{"note": "a b=c"}, false},
		{"no =", "app", nil, true},
		{"uppercase key", "App=x", nil, true},
		{"empty value", "app=", nil, true},
		{"value too long", "app=" + strings.Repeat("x", MetadataMaxValue+1), nil, true},
		{"non-ASCII value", "app=héllo", nil, true},
		{"too many pairs", strings.Repeat("a=b,", MetadataMaxPairs) + "a=b", nil, true},
	} {
		got, err := ParseMetadata(c.header)
		if (err != nil) != c.fails {
			t.Errorf("%s: err = %v, want failure %v", c.name, err, c.fails)
			continue
		}
		if !c.fails && !maps.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
