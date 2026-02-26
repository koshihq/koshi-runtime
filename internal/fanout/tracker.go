package fanout

import "context"

// FanoutStatus represents the current fan-out state for a correlation.
type FanoutStatus struct {
	CorrelationID string
	CallCount     int
	Depth         int
	MaxCalls      int
	MaxDepth      int
}

// Tracker tracks fan-out patterns for correlated requests.
type Tracker interface {
	Increment(ctx context.Context, correlationID string, depth int) (FanoutStatus, bool)
}

// NoOpTracker always allows requests. Stub for v1.
type NoOpTracker struct{}

// Increment always returns an empty status and allowed=true.
func (n *NoOpTracker) Increment(_ context.Context, correlationID string, _ int) (FanoutStatus, bool) {
	return FanoutStatus{CorrelationID: correlationID}, true
}
