package provider

import (
	"errors"
	"testing"
)

// ============================================================
// DetectProvider Tests
// ============================================================

func TestDetectProvider(t *testing.T) {
	tests := []struct {
		host     string
		expected Type
	}{
		{"api.openai.com", OpenAI},
		{"api.openai.com:443", OpenAI},
		{"API.OPENAI.COM", OpenAI},
		{"api.anthropic.com", Anthropic},
		{"api.anthropic.com:443", Anthropic},
		{"generativelanguage.googleapis.com", Google},
		{"unknown.example.com", Unknown},
		{"", Unknown},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got := DetectProvider(tt.host)
			if got != tt.expected {
				t.Errorf("DetectProvider(%q) = %d, want %d", tt.host, got, tt.expected)
			}
		})
	}
}

// ============================================================
// OpenAI Parser Tests
// ============================================================

func TestOpenAIParser_ValidUsage(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc123",
		"object": "chat.completion",
		"model": "gpt-4",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello"}}],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 20,
			"total_tokens": 30
		}
	}`)

	p := &OpenAIParser{}
	usage, err := p.ParseUsage(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage == nil {
		t.Fatal("expected usage data")
	}
	if usage.InputTokens != 10 {
		t.Errorf("expected input 10, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 20 {
		t.Errorf("expected output 20, got %d", usage.OutputTokens)
	}
	if usage.TotalTokens != 30 {
		t.Errorf("expected total 30, got %d", usage.TotalTokens)
	}
}

func TestOpenAIParser_NoUsage(t *testing.T) {
	body := []byte(`{"id": "chatcmpl-abc123", "choices": []}`)

	p := &OpenAIParser{}
	usage, err := p.ParseUsage(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage != nil {
		t.Errorf("expected nil usage, got %+v", usage)
	}
}

func TestOpenAIParser_MalformedJSON(t *testing.T) {
	p := &OpenAIParser{}
	_, err := p.ParseUsage([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestOpenAISSEUsage_Valid(t *testing.T) {
	data := []byte(`{"id":"chatcmpl-abc","choices":[],"usage":{"prompt_tokens":15,"completion_tokens":25,"total_tokens":40}}`)

	usage, isFinal, err := ParseOpenAISSEUsage(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isFinal {
		t.Error("expected final=true for chunk with usage")
	}
	if usage.TotalTokens != 40 {
		t.Errorf("expected 40 total, got %d", usage.TotalTokens)
	}
}

func TestOpenAISSEUsage_NoUsage(t *testing.T) {
	data := []byte(`{"id":"chatcmpl-abc","choices":[{"delta":{"content":"Hi"}}]}`)

	usage, isFinal, _ := ParseOpenAISSEUsage(data)
	if isFinal {
		t.Error("expected final=false for chunk without usage")
	}
	if usage != nil {
		t.Errorf("expected nil usage, got %+v", usage)
	}
}

func TestOpenAISSEUsage_NotJSON(t *testing.T) {
	usage, isFinal, _ := ParseOpenAISSEUsage([]byte("[DONE]"))
	if isFinal || usage != nil {
		t.Error("expected no result for non-JSON data")
	}
}

// ============================================================
// Anthropic Parser Tests
// ============================================================

func TestAnthropicParser_ValidUsage(t *testing.T) {
	body := []byte(`{
		"id": "msg_abc123",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello"}],
		"model": "claude-3-opus-20240229",
		"usage": {
			"input_tokens": 25,
			"output_tokens": 50
		}
	}`)

	p := &AnthropicParser{}
	usage, err := p.ParseUsage(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage == nil {
		t.Fatal("expected usage data")
	}
	if usage.InputTokens != 25 {
		t.Errorf("expected input 25, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("expected output 50, got %d", usage.OutputTokens)
	}
	if usage.TotalTokens != 75 {
		t.Errorf("expected total 75, got %d", usage.TotalTokens)
	}
}

func TestAnthropicParser_NoUsage(t *testing.T) {
	body := []byte(`{"id": "msg_abc123", "type": "message"}`)

	p := &AnthropicParser{}
	usage, err := p.ParseUsage(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage != nil {
		t.Errorf("expected nil usage, got %+v", usage)
	}
}

func TestAnthropicParser_MalformedJSON(t *testing.T) {
	p := &AnthropicParser{}
	_, err := p.ParseUsage([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// ============================================================
// Anthropic SSE Parser Tests
// ============================================================

func TestAnthropicSSEUsage_MessageStart(t *testing.T) {
	data := []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-3-opus-20240229","usage":{"input_tokens":42,"output_tokens":0}}}`)

	usage, isFinal, err := ParseAnthropicSSEUsage(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isFinal {
		t.Error("expected isFinal=false for message_start")
	}
	if usage == nil {
		t.Fatal("expected usage data")
	}
	if usage.InputTokens != 42 {
		t.Errorf("expected input_tokens 42, got %d", usage.InputTokens)
	}
}

func TestAnthropicSSEUsage_MessageDelta(t *testing.T) {
	data := []byte(`{"type":"message_delta","usage":{"output_tokens":73}}`)

	usage, isFinal, err := ParseAnthropicSSEUsage(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isFinal {
		t.Error("expected isFinal=true for message_delta")
	}
	if usage == nil {
		t.Fatal("expected usage data")
	}
	if usage.OutputTokens != 73 {
		t.Errorf("expected output_tokens 73, got %d", usage.OutputTokens)
	}
}

func TestAnthropicSSEUsage_ContentBlockDelta(t *testing.T) {
	data := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)

	usage, isFinal, _ := ParseAnthropicSSEUsage(data)
	if isFinal || usage != nil {
		t.Error("expected no result for content_block_delta")
	}
}

func TestAnthropicSSEUsage_MalformedJSON(t *testing.T) {
	usage, isFinal, _ := ParseAnthropicSSEUsage([]byte("not json"))
	if isFinal || usage != nil {
		t.Error("expected no result for malformed JSON")
	}
}

func TestAnthropicSSEUsage_MessageStartNoUsage(t *testing.T) {
	data := []byte(`{"type":"message_start","message":{"id":"msg_1"}}`)

	usage, isFinal, _ := ParseAnthropicSSEUsage(data)
	if isFinal || usage != nil {
		t.Error("expected no result for message_start without usage")
	}
}

func TestAnthropicSSEUsage_MessageDeltaNoUsage(t *testing.T) {
	data := []byte(`{"type":"message_delta"}`)

	usage, isFinal, _ := ParseAnthropicSSEUsage(data)
	if isFinal || usage != nil {
		t.Error("expected no result for message_delta without usage")
	}
}

// ============================================================
// Google Parser Tests
// ============================================================

func TestGoogleParser_NotImplemented(t *testing.T) {
	p := &GoogleParser{}
	_, err := p.ParseUsage([]byte(`{}`))
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("expected ErrNotImplemented, got: %v", err)
	}
}

// ============================================================
// DefaultMaxTokens Tests
// ============================================================

func TestDefaultMaxTokens(t *testing.T) {
	if DefaultMaxTokens(OpenAI) != 4096 {
		t.Errorf("expected 4096 for OpenAI")
	}
	if DefaultMaxTokens(Anthropic) != 8192 {
		t.Errorf("expected 8192 for Anthropic")
	}
	if DefaultMaxTokens(Google) != 2048 {
		t.Errorf("expected 2048 for Google")
	}
	if DefaultMaxTokens(Unknown) != 4096 {
		t.Errorf("expected 4096 for Unknown")
	}
}

// ============================================================
// GetParser Tests
// ============================================================

func TestGetParser(t *testing.T) {
	if _, ok := GetParser(OpenAI).(*OpenAIParser); !ok {
		t.Error("expected OpenAIParser for OpenAI")
	}
	if _, ok := GetParser(Anthropic).(*AnthropicParser); !ok {
		t.Error("expected AnthropicParser for Anthropic")
	}
	if _, ok := GetParser(Google).(*GoogleParser); !ok {
		t.Error("expected GoogleParser for Google")
	}
	if GetParser(Unknown) != nil {
		t.Error("expected nil for Unknown")
	}
}
