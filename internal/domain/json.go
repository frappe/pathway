package domain

import "github.com/bytedance/sonic"

// sonic.Marshal and sonic.Unmarshal are the codec for request bodies. A 1M-token body is 4MB of
// JSON, and encoding/json spends ~16ms writing it where sonic spends ~1. Keys sort as encoding/json
// sorts them, so a rewritten body reads as it did; what a RawMessage holds is written as it is,
// unvalidated, because every one here came out of this decoder or was written by the gateway itself.
func init() {
	sonic.ConfigDefault = sonic.Config{SortMapKeys: true, NoValidateJSONMarshaler: true}.Froze()
}
