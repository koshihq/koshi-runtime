package proxy

import (
	"encoding/json"
)

// extractMaxTokens extracts the max_tokens value from a request body.
// Returns 0 if not present or unparseable.
func extractMaxTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}

	var req struct {
		MaxTokens    *int64 `json:"max_tokens"`
		MaxNewTokens *int64 `json:"max_new_tokens"` // some providers use this
	}

	if err := json.Unmarshal(body, &req); err != nil {
		return 0
	}

	if req.MaxTokens != nil {
		return *req.MaxTokens
	}
	if req.MaxNewTokens != nil {
		return *req.MaxNewTokens
	}
	return 0
}

// isStreamingRequest checks if a request body has "stream": true.
func isStreamingRequest(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	var req struct {
		Stream *bool `json:"stream"`
	}

	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}

	return req.Stream != nil && *req.Stream
}

// injectStreamOptions injects stream_options.include_usage into an OpenAI
// request body for streaming usage extraction. Returns the modified body.
func injectStreamOptions(body []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, err
	}

	streamOpts := map[string]bool{"include_usage": true}
	optsBytes, err := json.Marshal(streamOpts)
	if err != nil {
		return body, err
	}

	raw["stream_options"] = optsBytes
	return json.Marshal(raw)
}
