package inject

import "encoding/json"

// JSONPatchOp represents a single RFC 6902 JSON Patch operation.
type JSONPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// MarshalPatches serializes a slice of patch operations to JSON.
func MarshalPatches(patches []JSONPatchOp) ([]byte, error) {
	return json.Marshal(patches)
}
