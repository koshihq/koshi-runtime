package budget

import (
	"sync"
	"sync/atomic"
	"time"
)

// nowFunc is overridable for testing.
var nowFunc = time.Now

type bucket struct {
	tokens int64
}

// RollingWindow is a time-bucketed circular buffer for tracking token usage.
// Each bucket represents one second. Expired buckets are lazily evicted.
type RollingWindow struct {
	mu          sync.Mutex
	buckets     []bucket
	numBuckets  int
	headIdx     int
	headTime    time.Time
	totalTokens int64 // cached incremental sum

	burstMax       int64
	burstRemaining atomic.Int64
}

// NewRollingWindow creates a rolling window with the given duration in seconds.
func NewRollingWindow(windowSeconds int, burstTokens int64) *RollingWindow {
	w := &RollingWindow{
		buckets:    make([]bucket, windowSeconds),
		numBuckets: windowSeconds,
		headTime:   nowFunc().Truncate(time.Second),
		burstMax:   burstTokens,
	}
	w.burstRemaining.Store(burstTokens)
	return w
}

// advance moves the head pointer to the current second, zeroing expired buckets.
// Must be called with mu held.
func (w *RollingWindow) advance() {
	now := nowFunc().Truncate(time.Second)

	if now.Before(w.headTime) || now.Equal(w.headTime) {
		return
	}

	elapsed := int(now.Sub(w.headTime) / time.Second)

	if elapsed >= w.numBuckets {
		// Entire window expired — zero everything.
		for i := range w.buckets {
			w.buckets[i].tokens = 0
		}
		w.totalTokens = 0
		w.headIdx = 0
		w.headTime = now
		return
	}

	// Zero buckets that have expired.
	for i := 0; i < elapsed; i++ {
		w.headIdx = (w.headIdx + 1) % w.numBuckets
		w.totalTokens -= w.buckets[w.headIdx].tokens
		w.buckets[w.headIdx].tokens = 0
	}

	w.headTime = now
}

// Add adds tokens to the current bucket.
func (w *RollingWindow) Add(tokens int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.advance()
	w.buckets[w.headIdx].tokens += tokens
	w.totalTokens += tokens

	// Clamp to prevent negative totals from reconciliation.
	if w.totalTokens < 0 {
		w.totalTokens = 0
	}
}

// Total returns the current total tokens in the window.
func (w *RollingWindow) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.advance()

	if w.totalTokens < 0 {
		return 0
	}
	return w.totalTokens
}

// BurstRemaining returns the current burst tokens available.
func (w *RollingWindow) BurstRemaining() int64 {
	return w.burstRemaining.Load()
}

// ConsumeBurst attempts to consume burst tokens. Returns true if successful.
func (w *RollingWindow) ConsumeBurst(tokens int64) bool {
	for {
		cur := w.burstRemaining.Load()
		if cur < tokens {
			return false
		}
		if w.burstRemaining.CompareAndSwap(cur, cur-tokens) {
			return true
		}
	}
}

// ReplenishBurst replenishes burst up to the max.
func (w *RollingWindow) ReplenishBurst(tokens int64) {
	for {
		cur := w.burstRemaining.Load()
		newVal := cur + tokens
		if newVal > w.burstMax {
			newVal = w.burstMax
		}
		if w.burstRemaining.CompareAndSwap(cur, newVal) {
			return
		}
	}
}
