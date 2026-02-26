package fanout

import (
	"context"
	"testing"
)

func TestNoOpTracker_AlwaysAllows(t *testing.T) {
	tr := &NoOpTracker{}
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		status, allowed := tr.Increment(ctx, "corr-1", i)
		if !allowed {
			t.Fatalf("expected allowed on call %d", i)
		}
		if status.CorrelationID != "corr-1" {
			t.Errorf("expected corr-1, got %s", status.CorrelationID)
		}
	}
}
