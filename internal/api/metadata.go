package api

import "encoding/json"

// extractStringField pulls a single string-valued top-level field out
// of a json.RawMessage blob. Returns the empty string if the blob is
// malformed, the field is missing, or the field is not a string.
//
// This is separate from convert.go so tests can exercise it in
// isolation and so the metadata-extraction rules are easy to spot
// when extending the snapshot shape.
func extractStringField(raw json.RawMessage, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[field]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
