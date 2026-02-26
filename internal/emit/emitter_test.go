package emit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLogEmitter_EventDelivered(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	e := NewLogEmitter(logger)

	e.Emit(context.Background(), Event{
		Type:       "enforcement",
		WorkloadID: "svc-a",
		Severity:   "info",
		Attributes: map[string]any{"action": "throttle"},
	})

	e.Close()

	output := buf.String()
	if !strings.Contains(output, "enforcement") {
		t.Errorf("expected enforcement in output, got: %s", output)
	}
	if !strings.Contains(output, "svc-a") {
		t.Errorf("expected svc-a in output, got: %s", output)
	}
}

func TestLogEmitter_BufferFull_Drop(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	e := &LogEmitter{
		events: make(chan Event, 2), // tiny buffer
		done:   make(chan struct{}),
		logger: logger,
	}
	// Don't start drain goroutine — buffer will fill.

	e.Emit(context.Background(), Event{Type: "a"})
	e.Emit(context.Background(), Event{Type: "b"})
	// Buffer full — next should drop.
	e.Emit(context.Background(), Event{Type: "c"})
	e.Emit(context.Background(), Event{Type: "d"})

	if e.Dropped() != 2 {
		t.Errorf("expected 2 dropped, got %d", e.Dropped())
	}

	// Start drain and close.
	go e.drain()
	close(e.events)
	<-e.done
}

func TestLogEmitter_NonBlocking(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	e := &LogEmitter{
		events: make(chan Event, 1), // tiny buffer
		done:   make(chan struct{}),
		logger: logger,
	}
	// Fill the buffer, don't drain.
	e.events <- Event{Type: "fill"}

	// Emit should not block — it should return almost instantly.
	done := make(chan struct{})
	go func() {
		e.Emit(context.Background(), Event{Type: "should_drop"})
		close(done)
	}()

	select {
	case <-done:
		// Good — Emit returned.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Emit blocked — non-blocking guarantee violated")
	}

	if e.Dropped() != 1 {
		t.Errorf("expected 1 dropped, got %d", e.Dropped())
	}

	// Cleanup.
	go e.drain()
	close(e.events)
	<-e.done
}

func TestLogEmitter_GracefulDrain(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	e := NewLogEmitter(logger)

	for i := 0; i < 50; i++ {
		e.Emit(context.Background(), Event{
			Type:       "test",
			WorkloadID: "svc-a",
			Severity:   "info",
		})
	}

	e.Close()

	output := buf.String()
	count := strings.Count(output, "\"event_type\":\"test\"")
	if count != 50 {
		t.Errorf("expected 50 events drained, got %d", count)
	}
}

func TestLogEmitter_SeverityLevels(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	e := NewLogEmitter(logger)

	e.Emit(context.Background(), Event{Type: "a", Severity: "info"})
	e.Emit(context.Background(), Event{Type: "b", Severity: "warn"})
	e.Emit(context.Background(), Event{Type: "c", Severity: "error"})

	e.Close()

	output := buf.String()
	if !strings.Contains(output, "INFO") {
		t.Error("expected INFO level")
	}
	if !strings.Contains(output, "WARN") {
		t.Error("expected WARN level")
	}
	if !strings.Contains(output, "ERROR") {
		t.Error("expected ERROR level")
	}
}
