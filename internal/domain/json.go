package domain

import (
	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/encoder"
)

// JSON is the codec for request bodies. A 1M-token body is 4MB of JSON, and encoding/json spends
// ~25ms validating and copying it where sonic spends ~5. Keys sort as encoding/json sorts them, so
// a rewritten body reads as it did; what a RawMessage holds is written as it is, unvalidated,
// because every one here came out of this decoder or was written by the gateway itself.
var JSON = sonic.Config{SortMapKeys: true, NoValidateJSONMarshaler: true}.Froze()

// EncodeJSON appends v to *buf as JSON would write it: the way to encode into a buffer that
// outlives one call. The options are JSON's, and the two must stay the same.
func EncodeJSON(buf *[]byte, v any) error {
	return encoder.EncodeInto(buf, v, encoder.SortMapKeys|encoder.NoValidateJSONMarshaler)
}
