package provider

import (
	"errors"
	"strings"
)

// Type represents a supported LLM provider.
type Type int

const (
	Unknown   Type = iota
	OpenAI         // api.openai.com
	Anthropic      // api.anthropic.com
	Google         // generativelanguage.googleapis.com
)

// UsageData represents extracted token usage from a provider response.
type UsageData struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// ErrNotImplemented is returned for providers not yet implemented.
var ErrNotImplemented = errors.New("provider not implemented in v1")

// Parser extracts usage data from provider responses.
type Parser interface {
	ParseUsage(body []byte) (*UsageData, error)
}

// Name returns the string name for a provider type.
func Name(pt Type) string {
	switch pt {
	case OpenAI:
		return "openai"
	case Anthropic:
		return "anthropic"
	case Google:
		return "google"
	default:
		return "unknown"
	}
}

// DetectProvider determines the provider type from the request host.
func DetectProvider(host string) Type {
	h := strings.ToLower(host)

	// Strip port if present.
	if idx := strings.LastIndex(h, ":"); idx != -1 {
		h = h[:idx]
	}

	switch {
	case strings.Contains(h, "openai.com"):
		return OpenAI
	case strings.Contains(h, "anthropic.com"):
		return Anthropic
	case strings.Contains(h, "googleapis.com"):
		return Google
	default:
		return Unknown
	}
}

// GetParser returns the parser for the given provider type.
func GetParser(pt Type) Parser {
	switch pt {
	case OpenAI:
		return &OpenAIParser{}
	case Anthropic:
		return &AnthropicParser{}
	case Google:
		return &GoogleParser{}
	default:
		return nil
	}
}

// DefaultMaxTokens returns the default max_tokens estimate for a provider
// when the request doesn't specify one.
func DefaultMaxTokens(pt Type) int64 {
	switch pt {
	case OpenAI:
		return 4096
	case Anthropic:
		return 8192
	case Google:
		return 2048
	default:
		return 4096
	}
}
