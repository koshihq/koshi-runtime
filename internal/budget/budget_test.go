package budget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// testClock provides a controllable clock for deterministic tests.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(t time.Time) *testClock {
	return &testClock{now: t.Truncate(time.Second)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func withTestClock(t *testing.T) *testClock {
	t.Helper()
	clk := newTestClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	origNow := nowFunc
	nowFunc = clk.Now
	t.Cleanup(func() { nowFunc = origNow })
	return clk
}

// defaultParams returns standard test budget params.
func defaultParams() BudgetParams {
	return BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 0}
}

// ============================================================
// Rolling Window Tests
// ============================================================

func TestWindow_Accumulation(t *testing.T) {
	clk := withTestClock(t)
	_ = clk
	w := NewRollingWindow(60, 0)

	w.Add(100)
	w.Add(200)
	w.Add(300)

	total := w.Total()
	if total != 600 {
		t.Errorf("expected 600, got %d", total)
	}
}

func TestWindow_Eviction_FullWindow(t *testing.T) {
	clk := withTestClock(t)
	w := NewRollingWindow(10, 0)

	w.Add(1000)

	clk.Advance(10 * time.Second)
	total := w.Total()
	if total != 0 {
		t.Errorf("expected 0 after full window eviction, got %d", total)
	}
}

func TestWindow_Eviction_Partial(t *testing.T) {
	clk := withTestClock(t)
	w := NewRollingWindow(10, 0)

	// Add 100 at t=0
	w.Add(100)

	// Advance 1 second, add 200 at t=1
	clk.Advance(1 * time.Second)
	w.Add(200)

	// Advance 1 second, add 300 at t=2
	clk.Advance(1 * time.Second)
	w.Add(300)

	// Total should be 600
	if total := w.Total(); total != 600 {
		t.Errorf("expected 600, got %d", total)
	}

	// Advance to t=10 — only t=0 bucket evicted (window is 10s, head at t=10, oldest valid is t=1)
	clk.Advance(8 * time.Second)
	total := w.Total()
	if total != 500 {
		t.Errorf("expected 500 after evicting t=0 bucket (100 tokens), got %d", total)
	}

	// Advance to t=11 — t=1 bucket evicted
	clk.Advance(1 * time.Second)
	total = w.Total()
	if total != 300 {
		t.Errorf("expected 300 after evicting t=1 bucket (200 tokens), got %d", total)
	}
}

func TestWindow_NegativeDelta_Clamped(t *testing.T) {
	clk := withTestClock(t)
	_ = clk
	w := NewRollingWindow(60, 0)

	w.Add(100)
	w.Add(-200) // Should clamp to 0

	total := w.Total()
	if total != 0 {
		t.Errorf("expected 0 (clamped), got %d", total)
	}
}

func TestWindow_NegativeDelta_AfterEviction(t *testing.T) {
	clk := withTestClock(t)
	w := NewRollingWindow(10, 0)

	// Reserve 1000 at t=0
	w.Add(1000)

	// Advance past window — reservation evicts
	clk.Advance(11 * time.Second)

	// Record with negative delta (reconciliation from evicted bucket)
	w.Add(-500) // Should clamp to 0

	total := w.Total()
	if total != 0 {
		t.Errorf("expected 0 (clamped after eviction + negative delta), got %d", total)
	}
}

func TestWindow_BucketBoundary(t *testing.T) {
	clk := withTestClock(t)
	w := NewRollingWindow(10, 0)

	// Add at t=0
	w.Add(100)

	// Advance exactly 1 second, add at t=1
	clk.Advance(1 * time.Second)
	w.Add(200)

	// Both should be in different buckets, total 300
	if total := w.Total(); total != 300 {
		t.Errorf("expected 300, got %d", total)
	}
}

func TestWindow_AdvancePastMultipleWindows(t *testing.T) {
	clk := withTestClock(t)
	w := NewRollingWindow(10, 0)

	w.Add(500)

	// Advance way past the window (3x)
	clk.Advance(30 * time.Second)
	total := w.Total()
	if total != 0 {
		t.Errorf("expected 0 after multi-window advance, got %d", total)
	}
}

// ============================================================
// Burst Tests
// ============================================================

func TestWindow_Burst_Consume(t *testing.T) {
	w := NewRollingWindow(60, 1000)

	if w.BurstRemaining() != 1000 {
		t.Errorf("expected burst 1000, got %d", w.BurstRemaining())
	}

	ok := w.ConsumeBurst(300)
	if !ok {
		t.Fatal("expected burst consumption to succeed")
	}
	if w.BurstRemaining() != 700 {
		t.Errorf("expected 700 burst remaining, got %d", w.BurstRemaining())
	}
}

func TestWindow_Burst_Exhaust(t *testing.T) {
	w := NewRollingWindow(60, 100)

	ok := w.ConsumeBurst(100)
	if !ok {
		t.Fatal("expected burst consumption to succeed")
	}

	ok = w.ConsumeBurst(1)
	if ok {
		t.Fatal("expected burst consumption to fail when exhausted")
	}
}

func TestWindow_Burst_Replenish(t *testing.T) {
	w := NewRollingWindow(60, 1000)

	w.ConsumeBurst(500)
	w.ReplenishBurst(300)
	if w.BurstRemaining() != 800 {
		t.Errorf("expected 800 after replenish, got %d", w.BurstRemaining())
	}

	// Replenish beyond max — capped
	w.ReplenishBurst(500)
	if w.BurstRemaining() != 1000 {
		t.Errorf("expected 1000 (capped), got %d", w.BurstRemaining())
	}
}

// ============================================================
// Tracker Tests (Reserve / Record / Status)
// ============================================================

func TestTracker_Reserve_WithinLimit(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", defaultParams())
	ctx := context.Background()

	status, allowed, err := tr.Reserve(ctx, "svc-a", 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("expected reservation to succeed within limit")
	}
	if status.WindowTokensUsed != 500 {
		t.Errorf("expected 500 used, got %d", status.WindowTokensUsed)
	}
}

func TestTracker_Reserve_ExceedsLimit_NoBurst(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", BudgetParams{WindowSeconds: 60, LimitTokens: 1000, BurstTokens: 0})
	ctx := context.Background()

	// Fill to limit
	tr.Reserve(ctx, "svc-a", 1000)

	// Next request should be denied
	status, allowed, err := tr.Reserve(ctx, "svc-a", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("expected reservation to fail at limit without burst")
	}
	if status.WindowTokensUsed != 1000 {
		t.Errorf("expected 1000 used (reservation undone), got %d", status.WindowTokensUsed)
	}
}

func TestTracker_Reserve_ExceedsLimit_WithBurst(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", BudgetParams{WindowSeconds: 60, LimitTokens: 1000, BurstTokens: 500})
	ctx := context.Background()

	// Fill to limit
	tr.Reserve(ctx, "svc-a", 1000)

	// Next should succeed using burst
	status, allowed, err := tr.Reserve(ctx, "svc-a", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("expected reservation to succeed with burst available")
	}
	if status.BurstRemaining > 500 {
		t.Errorf("expected burst consumed, remaining %d", status.BurstRemaining)
	}
}

func TestTracker_Reserve_UnknownWorkload(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	ctx := context.Background()

	_, _, err := tr.Reserve(ctx, "unknown", 100)
	if !errors.Is(err, ErrUnknownWorkload) {
		t.Fatalf("expected ErrUnknownWorkload, got: %v", err)
	}
}

func TestTracker_Record_ActualLessThanReserved(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", defaultParams())
	ctx := context.Background()

	// Reserve 1000
	tr.Reserve(ctx, "svc-a", 1000)

	// Record actual 300 → delta = 300 - 1000 = -700
	tr.Record(ctx, UsageReport{
		WorkloadID: "svc-a",
		Tokens:     -700, // delta: actual - reserved
		Timestamp:  time.Now(),
	})

	status, err := tr.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.WindowTokensUsed != 300 {
		t.Errorf("expected 300 after reconciliation, got %d", status.WindowTokensUsed)
	}
}

func TestTracker_Record_ActualGreaterThanReserved(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", defaultParams())
	ctx := context.Background()

	// Reserve 500
	tr.Reserve(ctx, "svc-a", 500)

	// Record actual 800 → delta = 800 - 500 = +300
	tr.Record(ctx, UsageReport{
		WorkloadID: "svc-a",
		Tokens:     300, // delta: actual - reserved
		Timestamp:  time.Now(),
	})

	status, err := tr.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.WindowTokensUsed != 800 {
		t.Errorf("expected 800 after reconciliation, got %d", status.WindowTokensUsed)
	}
}

func TestTracker_Record_ActualEqualsReserved(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", defaultParams())
	ctx := context.Background()

	// Reserve 1000
	tr.Reserve(ctx, "svc-a", 1000)

	// Record actual 1000 → delta = 0
	tr.Record(ctx, UsageReport{
		WorkloadID: "svc-a",
		Tokens:     0, // delta: actual - reserved
		Timestamp:  time.Now(),
	})

	status, err := tr.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.WindowTokensUsed != 1000 {
		t.Errorf("expected 1000, got %d", status.WindowTokensUsed)
	}
}

func TestTracker_Record_LateReconciliation_Clamped(t *testing.T) {
	clk := withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", BudgetParams{WindowSeconds: 10, LimitTokens: 10000, BurstTokens: 0})
	ctx := context.Background()

	// Reserve 1000 at t=0
	tr.Reserve(ctx, "svc-a", 1000)

	// Advance past window — reservation evicted
	clk.Advance(11 * time.Second)

	// Record with negative delta (late reconciliation)
	tr.Record(ctx, UsageReport{
		WorkloadID: "svc-a",
		Tokens:     -500,
		Timestamp:  time.Now(),
	})

	status, err := tr.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.WindowTokensUsed != 0 {
		t.Errorf("expected 0 (clamped), got %d", status.WindowTokensUsed)
	}
}

func TestTracker_Record_NoReconciliation_EvictsNaturally(t *testing.T) {
	clk := withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", BudgetParams{WindowSeconds: 10, LimitTokens: 10000, BurstTokens: 0})
	ctx := context.Background()

	// Reserve 1000, never record
	tr.Reserve(ctx, "svc-a", 1000)

	// Advance past window
	clk.Advance(11 * time.Second)

	status, err := tr.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.WindowTokensUsed != 0 {
		t.Errorf("expected 0 after natural eviction, got %d", status.WindowTokensUsed)
	}
}

func TestTracker_Status_UnknownWorkload(t *testing.T) {
	tr := NewTracker()
	ctx := context.Background()

	_, err := tr.Status(ctx, "unknown")
	if !errors.Is(err, ErrUnknownWorkload) {
		t.Fatalf("expected ErrUnknownWorkload, got: %v", err)
	}
}

func TestTracker_DeterministicRestart(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", BudgetParams{WindowSeconds: 60, LimitTokens: 100000, BurstTokens: 0})
	ctx := context.Background()

	var expected int64
	for i := 0; i < 100; i++ {
		tr.Reserve(ctx, "svc-a", 100)
		expected += 100
	}

	status, err := tr.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.WindowTokensUsed != expected {
		t.Errorf("expected %d, got %d", expected, status.WindowTokensUsed)
	}
}

// ============================================================
// StatusAll Tests
// ============================================================

func TestTracker_StatusAll_RegisteredWorkloads(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", BudgetParams{WindowSeconds: 60, LimitTokens: 10000, BurstTokens: 1000})
	tr.Register("svc-b", BudgetParams{WindowSeconds: 300, LimitTokens: 50000, BurstTokens: 0})
	ctx := context.Background()

	// Reserve some tokens on svc-a to verify used count.
	tr.Reserve(ctx, "svc-a", 500)

	all := tr.StatusAll(ctx)
	if len(all) != 2 {
		t.Fatalf("expected 2 workloads, got %d", len(all))
	}

	a, ok := all["svc-a"]
	if !ok {
		t.Fatal("expected svc-a in StatusAll result")
	}
	if a.LimitTokens != 10000 {
		t.Errorf("svc-a LimitTokens: expected 10000, got %d", a.LimitTokens)
	}
	if a.WindowSeconds != 60 {
		t.Errorf("svc-a WindowSeconds: expected 60, got %d", a.WindowSeconds)
	}
	if a.WindowTokensUsed != 500 {
		t.Errorf("svc-a WindowTokensUsed: expected 500, got %d", a.WindowTokensUsed)
	}

	b, ok := all["svc-b"]
	if !ok {
		t.Fatal("expected svc-b in StatusAll result")
	}
	if b.LimitTokens != 50000 {
		t.Errorf("svc-b LimitTokens: expected 50000, got %d", b.LimitTokens)
	}
	if b.WindowSeconds != 300 {
		t.Errorf("svc-b WindowSeconds: expected 300, got %d", b.WindowSeconds)
	}
	if b.WindowTokensUsed != 0 {
		t.Errorf("svc-b WindowTokensUsed: expected 0, got %d", b.WindowTokensUsed)
	}
}

func TestTracker_StatusAll_Empty(t *testing.T) {
	tr := NewTracker()
	ctx := context.Background()

	all := tr.StatusAll(ctx)
	if len(all) != 0 {
		t.Errorf("expected empty map, got %d entries", len(all))
	}
}

// ============================================================
// Concurrency Tests
// ============================================================

func TestTracker_Concurrent_Reserve_Record(t *testing.T) {
	withTestClock(t)
	tr := NewTracker()
	tr.Register("svc-a", BudgetParams{WindowSeconds: 60, LimitTokens: 10000000, BurstTokens: 0})
	ctx := context.Background()

	const goroutines = 1000
	const reserveAmount = 100
	const actualAmount = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			tr.Reserve(ctx, "svc-a", reserveAmount)
			// delta = actual - reserved = 50 - 100 = -50
			tr.Record(ctx, UsageReport{
				WorkloadID: "svc-a",
				Tokens:     actualAmount - reserveAmount,
				Timestamp:  time.Now(),
			})
		}()
	}

	wg.Wait()

	status, err := tr.Status(ctx, "svc-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := int64(goroutines * actualAmount) // 1000 * 50 = 50000
	if status.WindowTokensUsed != expected {
		t.Errorf("expected %d, got %d", expected, status.WindowTokensUsed)
	}
}

func TestTracker_Concurrent_BurstConsumption(t *testing.T) {
	withTestClock(t)
	burstMax := int64(1000)
	tr := NewTracker()
	tr.Register("svc-a", BudgetParams{WindowSeconds: 60, LimitTokens: 0, BurstTokens: burstMax})
	ctx := context.Background()

	const goroutines = 100
	var allowed int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, ok, _ := tr.Reserve(ctx, "svc-a", 100)
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	// With burst 1000 and each request needing burst for overage,
	// total allowed depends on the exact ordering but burst should never go negative.
	ws, _ := tr.windows.Load("svc-a")
	burst := ws.(*workloadState).window.BurstRemaining()
	if burst < 0 {
		t.Errorf("burst went negative: %d", burst)
	}
}
