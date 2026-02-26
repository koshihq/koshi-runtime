package provider

import (
	"encoding/json"
	"fmt"
)

// AnthropicParser parses usage data from Anthropic API responses.
type AnthropicParser struct{}

// anthropicResponse represents the relevant parts of an Anthropic messages response.
type anthropicResponse struct {
	Usage *anthropicUsage `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// ParseUsage extracts token usage from an Anthropic JSON response body.
func (p *AnthropicParser) ParseUsage(body []byte) (*UsageData, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("anthropic: parse response: %w", err)
	}

	if resp.Usage == nil {
		return nil, nil
	}

	return &UsageData{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
	}, nil
}

// anthropicSSEEvent is the minimal structure for detecting Anthropic SSE event types.
type anthropicSSEEvent struct {
	Type    string                  `json:"type"`
	Message *anthropicSSEMessage    `json:"message,omitempty"`
	Usage   *anthropicUsage         `json:"usage,omitempty"`
}

type anthropicSSEMessage struct {
	Usage *anthropicUsage `json:"usage,omitempty"`
}

// ParseAnthropicSSEUsage parses a single Anthropic SSE data line for usage info.
//
// Anthropic streaming has two usage-carrying events:
//   - message_start: {"type":"message_start","message":{"usage":{"input_tokens":N,...}}}
//   - message_delta: {"type":"message_delta","usage":{"output_tokens":N}}
//
// Returns (usage, isFinal, err):
//   - message_start: (*UsageData{InputTokens: N}, false, nil)
//   - message_delta: (*UsageData{OutputTokens: N}, true, nil)
//   - other events or parse failure: (nil, false, nil)
func ParseAnthropicSSEUsage(data []byte) (*UsageData, bool, error) {
	var ev anthropicSSEEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, false, nil
	}

	switch ev.Type {
	case "message_start":
		if ev.Message == nil || ev.Message.Usage == nil {
			return nil, false, nil
		}
		return &UsageData{
			InputTokens: ev.Message.Usage.InputTokens,
		}, false, nil

	case "message_delta":
		if ev.Usage == nil {
			return nil, false, nil
		}
		return &UsageData{
			OutputTokens: ev.Usage.OutputTokens,
		}, true, nil

	default:
		return nil, false, nil
	}
}
