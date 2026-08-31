package proxy

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const (
	vendorID = "claude-sonnet-4-5-20250929"
	groveID  = "anthropic/claude-4-5"
)

func swapped(t *testing.T, input string, wrap func(io.Reader) io.Reader) string {
	t.Helper()
	var src io.Reader = strings.NewReader(input)
	if wrap != nil {
		src = wrap(src)
	}
	out, err := io.ReadAll(newModelSwapReader(io.NopCloser(src), ModelSwap{Upstream: vendorID, Client: groveID}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

func TestTheModelFieldIsSwappedBackToTheClientsID(t *testing.T) {
	for name, input := range map[string]string{
		"compact":     `{"id":"msg_1","model":"` + vendorID + `","stop_reason":null}`,
		"spaced":      `{"id":"msg_1","model": "` + vendorID + `","stop_reason":null}`,
		"every frame": `data: {"model":"` + vendorID + `"}` + "\n\n" + `data: {"model":"` + vendorID + `"}` + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			got := swapped(t, input, nil)
			if !strings.Contains(got, groveID) || strings.Contains(got, vendorID) {
				t.Errorf("swap missed: %q", got)
			}
		})
	}
}

// The field split across arbitrary read boundaries is the case the carry exists for.
func TestAFieldSplitAcrossReadsIsStillSwapped(t *testing.T) {
	input := `{"model":"` + vendorID + `","x":1}`
	got := swapped(t, input, func(r io.Reader) io.Reader { return iotest.OneByteReader(r) })

	if got != `{"model":"`+groveID+`","x":1}` {
		t.Errorf("got %q", got)
	}
}

// A model quoting the vendor's name inside generated text arrives escaped (\"model\") and must
// pass through untouched — only the real field matches raw unescaped bytes.
func TestGeneratedTextMentioningTheVendorIDIsLeftAlone(t *testing.T) {
	input := `{"delta":{"content":"{\"model\":\"` + vendorID + `\"}"},"note":"` + vendorID + `"}`
	if got := swapped(t, input, nil); got != input {
		t.Errorf("content was rewritten: %q", got)
	}
}

// Everything that is not the model field survives byte-for-byte.
func TestBytesAroundTheFieldAreUntouched(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"He"}}],"model":"` + vendorID + `"}` + "\n\n"
	want := strings.ReplaceAll(input, vendorID, groveID)
	if got := swapped(t, input, nil); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
