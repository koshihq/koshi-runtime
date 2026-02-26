package proxy

import "github.com/koshihq/koshi-runtime/internal/provider"

// anthropicSSEAccumulator accumulates Anthropic SSE usage across two events
// (message_start and message_delta) and produces a single UsageData on
// finalization. One instance per request — not shared across streams.
//
// Lifecycle contract:
//   - tokenExtractingReader calls Parse for each SSE data line
//   - Parse returns (nil, false, nil) for all non-final events
//   - Parse returns (*UsageData, true, nil) only on message_delta when
//     both input and output tokens have been observed
//   - tokenExtractingReader stores the final UsageData and fires onUsage
//     from Close() — the only reconciliation point
//
// Fail-safe: if message_delta arrives without a prior message_start,
// Parse returns (nil, false, nil). No UsageData is produced, onUsage
// never fires, and the reservation stands. Overcounting is always
// preferable to undercounting.
type anthropicSSEAccumulator struct {
	inputTokens int64
	hasInput    bool
}

// Parse processes a single Anthropic SSE data line. It delegates JSON parsing
// to provider.ParseAnthropicSSEUsage and accumulates state across events.
func (a *anthropicSSEAccumulator) Parse(data []byte) (*provider.UsageData, bool, error) {
	usage, isFinal, err := provider.ParseAnthropicSSEUsage(data)
	if err != nil {
		return nil, false, nil
	}

	if usage == nil {
		return nil, false, nil
	}

	if !isFinal {
		// message_start: capture input tokens, do not report yet.
		a.inputTokens = usage.InputTokens
		a.hasInput = true
		return nil, false, nil
	}

	// message_delta (isFinal=true): produce complete UsageData only if
	// we have both halves. If message_start was missing, fail safe.
	if !a.hasInput {
		return nil, false, nil
	}

	return &provider.UsageData{
		InputTokens:  a.inputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  a.inputTokens + usage.OutputTokens,
	}, true, nil
}
