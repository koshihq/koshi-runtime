package provider

import (
	"encoding/json"
	"fmt"
)

// OpenAIParser parses usage data from OpenAI API responses.
type OpenAIParser struct{}

// openAIResponse represents the relevant parts of an OpenAI chat completion response.
type openAIResponse struct {
	Usage *openAIUsage `json:"usage"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// ParseUsage extracts token usage from an OpenAI JSON response body.
func (p *OpenAIParser) ParseUsage(body []byte) (*UsageData, error) {
	var resp openAIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("openai: parse response: %w", err)
	}

	if resp.Usage == nil {
		return nil, nil // No usage data in response
	}

	return &UsageData{
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
		TotalTokens:  resp.Usage.TotalTokens,
	}, nil
}

// ParseSSEUsage extracts usage from an OpenAI SSE data line.
// Returns the usage data (if found), whether this is the final usage chunk, and any error.
func ParseOpenAISSEUsage(data []byte) (*UsageData, bool, error) {
	var chunk struct {
		Usage *openAIUsage `json:"usage"`
	}

	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, false, nil // Not a JSON chunk we care about
	}

	if chunk.Usage == nil {
		return nil, false, nil
	}

	return &UsageData{
		InputTokens:  chunk.Usage.PromptTokens,
		OutputTokens: chunk.Usage.CompletionTokens,
		TotalTokens:  chunk.Usage.TotalTokens,
	}, true, nil
}
