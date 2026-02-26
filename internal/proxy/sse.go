package proxy

import (
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/koshihq/koshi-runtime/internal/provider"
)

const maxLineBuffer = 8192 // 8KB max line buffer

// sseParseFunc is the signature for provider-specific SSE usage parsers.
// Returns (usage, isFinal, err). isFinal=true signals the last usage event.
type sseParseFunc func(data []byte) (*provider.UsageData, bool, error)

// tokenExtractingReader wraps an SSE stream body and extracts usage data
// while passing all bytes through unchanged. Provider-agnostic: the parsing
// logic is injected via parseFunc.
type tokenExtractingReader struct {
	inner     io.ReadCloser
	buf       []byte // partial line buffer
	usage     *provider.UsageData
	parseFunc sseParseFunc
	onUsage   func(usage *provider.UsageData)
	mu        sync.Mutex
	closed    bool
}

// newTokenExtractingReader wraps an SSE response body for usage extraction.
// parseFunc determines how individual SSE data lines are parsed (provider-specific).
func newTokenExtractingReader(inner io.ReadCloser, parseFunc sseParseFunc, onUsage func(usage *provider.UsageData)) *tokenExtractingReader {
	return &tokenExtractingReader{
		inner:     inner,
		parseFunc: parseFunc,
		onUsage:   onUsage,
	}
}

// Read passes bytes through unchanged while scanning for usage data.
// Implements io.Reader.
func (r *tokenExtractingReader) Read(p []byte) (n int, err error) {
	// Panic safety: recover inside Read to prevent propagation.
	defer func() {
		if rec := recover(); rec != nil {
			// On panic, signal EOF and let the stream close cleanly.
			n = 0
			err = io.EOF
		}
	}()

	n, err = r.inner.Read(p)
	if n > 0 {
		r.scan(p[:n])
	}
	return n, err
}

// Close closes the underlying reader and reports any extracted usage.
func (r *tokenExtractingReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	usage := r.usage
	r.mu.Unlock()

	closeErr := r.inner.Close()

	if usage != nil && r.onUsage != nil {
		r.onUsage(usage)
	}

	return closeErr
}

// scan examines bytes for SSE data lines containing usage information.
func (r *tokenExtractingReader) scan(data []byte) {
	// Append to existing partial line buffer.
	r.buf = append(r.buf, data...)

	for {
		// Find the next newline.
		idx := bytes.IndexByte(r.buf, '\n')
		if idx == -1 {
			// No complete line yet. Truncate if buffer exceeds max.
			if len(r.buf) > maxLineBuffer {
				r.buf = r.buf[:0]
			}
			return
		}

		line := r.buf[:idx]
		r.buf = r.buf[idx+1:]

		r.processLine(line)
	}
}

// Sentinel bytes for fast-path filtering. Content block deltas (the majority
// of SSE lines) contain neither — they are skipped without JSON parsing.
var (
	sentinelUsage   = []byte(`"usage"`)
	sentinelMsgType = []byte(`"type":"message_"`)
)

// processLine handles a single SSE line, looking for usage data.
func (r *tokenExtractingReader) processLine(line []byte) {
	// Trim trailing \r for \r\n line endings.
	line = bytes.TrimRight(line, "\r")

	// SSE data lines start with "data: ".
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return
	}

	payload := line[6:] // Strip "data: " prefix.

	// Skip the [DONE] sentinel.
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return
	}

	// Fast-path: skip lines that cannot contain usage data.
	// "usage" covers OpenAI chunks and Anthropic message_start/message_delta.
	// "type":"message_" catches Anthropic event types as a safety net.
	if !bytes.Contains(payload, sentinelUsage) && !bytes.Contains(payload, sentinelMsgType) {
		return
	}

	usage, isFinal, _ := r.parseFunc(payload)
	if isFinal && usage != nil {
		r.mu.Lock()
		r.usage = usage
		r.mu.Unlock()
	}
}

// isSSEResponse checks if the response is a Server-Sent Events stream.
func isSSEResponse(contentType string) bool {
	return strings.Contains(contentType, "text/event-stream")
}
