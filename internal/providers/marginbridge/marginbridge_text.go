// marginbridge_text.go — small JSON text-shaping helpers used by the wake
// text builder and the self-throttle snapshot cache.
package marginbridge

import "encoding/json"

// countEntries mirrors the prototype's entry count for a margin receipt
// file: `len(data) if isinstance(data, list) else len(data.get("comments",
// [data]))`. Returns 0 if the content isn't valid JSON.
func countEntries(text string) int {
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(text), &arr); err == nil {
		return len(arr)
	}
	var obj struct {
		Comments []json.RawMessage `json:"comments"`
	}
	if err := json.Unmarshal([]byte(text), &obj); err == nil {
		if obj.Comments != nil {
			return len(obj.Comments)
		}
		return 1 // matches the prototype's `[data]` fallback: the object itself counts as one entry.
	}
	return 0
}

// extractTextField mirrors the prototype's `str(data.get("text", ""))` for
// a general signal file. Returns "" if the content isn't a JSON object or
// has no text field.
func extractTextField(text string) string {
	var obj struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return ""
	}
	return obj.Text
}

// marshalSnapshot serializes a liveSnapshot for the self-throttle cache
// persisted in reconcile.State.Metadata["snapshot_json"].
func marshalSnapshot(snap *liveSnapshot) (string, error) {
	data, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
