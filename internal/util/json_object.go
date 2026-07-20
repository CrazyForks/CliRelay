package util

import (
	"bytes"
	"encoding/json"

	"github.com/tidwall/gjson"
)

// MutateTopLevelObject rewrites a JSON object in one pass: apply top-level sets and deletes.
// values in set must be raw JSON (e.g. true, "x", [1], {...}).
// Nested paths are not supported; callers should mutate nested fragments first.
//
// This avoids N full-document copies from repeated sjson.Set/Delete on large bodies.
func MutateTopLevelObject(src []byte, set map[string][]byte, del []string) []byte {
	src = bytes.TrimSpace(src)
	if len(src) == 0 {
		src = []byte(`{}`)
	}
	root := gjson.ParseBytes(src)
	if !root.IsObject() {
		return src
	}

	delSet := make(map[string]struct{}, len(del))
	for _, k := range del {
		if k != "" {
			delSet[k] = struct{}{}
		}
	}
	pending := make(map[string][]byte, len(set))
	for k, v := range set {
		if k == "" {
			continue
		}
		pending[k] = v
	}

	var b bytes.Buffer
	b.Grow(len(src) + 256)
	b.WriteByte('{')
	first := true
	writePair := func(key string, raw []byte) {
		if len(raw) == 0 {
			raw = []byte("null")
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		keyJSON, err := json.Marshal(key)
		if err != nil {
			return
		}
		b.Write(keyJSON)
		b.WriteByte(':')
		b.Write(raw)
	}

	root.ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		if _, drop := delSet[k]; drop {
			delete(pending, k)
			return true
		}
		if raw, ok := pending[k]; ok {
			writePair(k, raw)
			delete(pending, k)
			return true
		}
		writePair(k, []byte(value.Raw))
		return true
	})
	for k, raw := range pending {
		if _, drop := delSet[k]; drop {
			continue
		}
		writePair(k, raw)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// JSONString returns a JSON string literal for s.
func JSONString(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		return []byte(`""`)
	}
	return b
}

// JSONBool returns raw JSON true/false.
func JSONBool(v bool) []byte {
	if v {
		return []byte("true")
	}
	return []byte("false")
}
