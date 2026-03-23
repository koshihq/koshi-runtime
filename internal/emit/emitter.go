package emit

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// EventBudgetReconciled is emitted when a reserve-to-record delta is applied.
const EventBudgetReconciled = "budget_reconciled"

// Event represents a telemetry event.
type Event struct {
	Type       string
	WorkloadID string
	Attributes map[string]any
	Severity   string // "info", "warn", "error"
}

// Emitter emits telemetry events.
type Emitter interface {
	Emit(ctx context.Context, event Event)
	Dropped() int64
	Close()
}

const defaultBufferSize = 1000

// LogEmitter emits events as structured JSON logs via slog.
// Events are buffered and never block the proxy path.
type LogEmitter struct {
	events  chan Event
	dropped atomic.Int64
	done    chan struct{}
	logger  *slog.Logger
}

// NewLogEmitter creates a new LogEmitter that writes to the given logger.
// The logger should have a "stream" attribute set to "event" to distinguish
// structured events from runtime logs.
func NewLogEmitter(logger *slog.Logger) *LogEmitter {
	e := &LogEmitter{
		events: make(chan Event, defaultBufferSize),
		done:   make(chan struct{}),
		logger: logger,
	}
	go e.drain()
	return e
}

// Emit sends an event to the buffer. Never blocks. Drops if buffer full.
func (e *LogEmitter) Emit(_ context.Context, event Event) {
	select {
	case e.events <- event:
	default:
		e.dropped.Add(1)
	}
}

// Dropped returns the number of dropped events.
func (e *LogEmitter) Dropped() int64 {
	return e.dropped.Load()
}

// Close drains remaining events and stops the emitter.
func (e *LogEmitter) Close() {
	close(e.events)
	<-e.done
}

func (e *LogEmitter) drain() {
	defer close(e.done)
	for event := range e.events {
		attrs := []any{
			"event_type", event.Type,
			"workload_id", event.WorkloadID,
			"severity", event.Severity,
		}
		for k, v := range event.Attributes {
			attrs = append(attrs, k, v)
		}

		switch event.Severity {
		case "error":
			e.logger.Error("koshi event", attrs...)
		case "warn":
			e.logger.Warn("koshi event", attrs...)
		default:
			e.logger.Info("koshi event", attrs...)
		}
	}
}
