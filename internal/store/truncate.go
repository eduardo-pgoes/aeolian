package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Limits on what the log keeps of a hook's input.
const (
	HookPayloadLimit  = 8 << 10
	ToolInputLimit    = 1 << 10
	HumanMessageLimit = 8 << 10
)

// Truncate returns payload cut to at most about limit bytes, keeping it
// JSON: string fields over a shrinking cap are cut and end in "…[N bytes]",
// N being their full length. A payload that is not JSON is kept as a cut
// string; one that still does not fit, say a huge array of short strings,
// becomes {"truncated":true,"bytes":N}.
func Truncate(payload []byte, limit int) json.RawMessage {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil
	}
	if len(payload) <= limit && json.Valid(payload) {
		return json.RawMessage(payload)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || dec.More() {
		v = string(payload)
	}
	for limitCap := limit / 2; limitCap >= 16; limitCap /= 2 {
		b, err := json.Marshal(cutStrings(v, limitCap))
		if err == nil && len(b) <= limit {
			return b
		}
	}
	b, _ := json.Marshal(map[string]any{"truncated": true, "bytes": len(payload)})
	return b
}

func cutStrings(v any, limit int) any {
	switch v := v.(type) {
	case string:
		return CutString(v, limit)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = cutStrings(e, limit)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = cutStrings(e, limit)
		}
		return out
	}
	return v
}

// CutString returns s when it fits in limit bytes, and otherwise its
// longest prefix that ends on a rune boundary followed by "…[N bytes]",
// N being len(s), all within limit bytes. A limit too small for the
// suffix gets the suffix alone.
func CutString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	suffix := fmt.Sprintf("…[%d bytes]", len(s))
	cut := max(limit-len(suffix), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}
