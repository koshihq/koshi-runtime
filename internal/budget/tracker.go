package budget

import (
	"context"
	"errors"
	"sync"
	"time"
)

// UsageReport represents actual token usage from a provider response.
type UsageReport struct {
	WorkloadID string
	Tokens     int64
	Timestamp  time.Time
	IsEstimate bool
}

// BudgetStatus represents the current budget state for a workload.
type BudgetStatus struct {
	WindowTokensUsed  int64
	WindowTokensLimit int64
	BurstRemaining    int64
}

// BudgetParams holds the budget configuration for a single workload.
type BudgetParams struct {
	WindowSeconds int
	LimitTokens   int64
	BurstTokens   int64
}

// WorkloadStatus represents the full budget state for a workload,
// used by the /status diagnostic endpoint.
type WorkloadStatus struct {
	LimitTokens      int64
	WindowTokensUsed int64
	BurstRemaining   int64
	WindowSeconds    int
}

// ErrUnknownWorkload is returned when an operation targets a workload
// that was not registered at startup. This signals a configuration gap,
// not a budget denial.
var ErrUnknownWorkload = errors.New("budget: unknown workload (not registered)")

// Tracker manages token budgets for workloads.
type Tracker interface {
	Reserve(ctx context.Context, workloadID string, estimatedTokens int64) (BudgetStatus, bool, error)
	Record(ctx context.Context, report UsageReport)
	Status(ctx context.Context, workloadID string) (BudgetStatus, error)
	StatusAll(ctx context.Context) map[string]WorkloadStatus
}

type workloadState struct {
	window        *RollingWindow
	windowSeconds int
	limitTokens   int64
}

// TrackerImpl is the in-memory budget tracker.
type TrackerImpl struct {
	windows sync.Map // map[string]*workloadState
}

// NewTracker creates a new budget tracker.
// Workloads must be registered via Register() before use.
func NewTracker() *TrackerImpl {
	return &TrackerImpl{}
}

// Register creates budget state for a workload with the given params.
// Must be called at startup before any Reserve calls. Uses LoadOrStore
// so concurrent calls for the same workloadID are safe — the first wins.
func (t *TrackerImpl) Register(workloadID string, params BudgetParams) {
	ws := &workloadState{
		window:        NewRollingWindow(params.WindowSeconds, params.BurstTokens),
		windowSeconds: params.WindowSeconds,
		limitTokens:   params.LimitTokens,
	}
	t.windows.LoadOrStore(workloadID, ws)
}

func (t *TrackerImpl) get(workloadID string) (*workloadState, error) {
	v, ok := t.windows.Load(workloadID)
	if !ok {
		return nil, ErrUnknownWorkload
	}
	return v.(*workloadState), nil
}

// Reserve attempts to reserve estimated tokens for a workload request.
// Returns the current budget status, whether the reservation was allowed,
// and an error if the workload is not registered.
func (t *TrackerImpl) Reserve(_ context.Context, workloadID string, estimatedTokens int64) (BudgetStatus, bool, error) {
	ws, err := t.get(workloadID)
	if err != nil {
		return BudgetStatus{}, false, err
	}

	// Add the reservation to the window.
	ws.window.Add(estimatedTokens)

	total := ws.window.Total()
	burst := ws.window.BurstRemaining()

	status := BudgetStatus{
		WindowTokensUsed:  total,
		WindowTokensLimit: ws.limitTokens,
		BurstRemaining:    burst,
	}

	if total <= ws.limitTokens {
		return status, true, nil
	}

	// Over limit — try to use burst.
	overage := total - ws.limitTokens
	if ws.window.ConsumeBurst(overage) {
		status.BurstRemaining = ws.window.BurstRemaining()
		return status, true, nil
	}

	// Cannot reserve — undo the reservation.
	ws.window.Add(-estimatedTokens)
	status.WindowTokensUsed = ws.window.Total()
	status.BurstRemaining = ws.window.BurstRemaining()
	return status, false, nil
}

// Record records actual token usage after a response, reconciling with the reservation.
func (t *TrackerImpl) Record(_ context.Context, report UsageReport) {
	v, ok := t.windows.Load(report.WorkloadID)
	if !ok {
		// No window for this workload — nothing to reconcile.
		return
	}
	ws := v.(*workloadState)

	// Delta is actual - 0 since the reservation is already in the window.
	// The caller tells us the actual tokens; we need to compute delta
	// from (actual - reserved). But we don't track per-request reservation.
	// Instead, Record is called with the delta to apply.
	// Convention: report.Tokens is the delta (actual - reserved).
	// If actual < reserved, delta is negative → tokens returned to budget.
	ws.window.Add(report.Tokens)

	// Replenish burst if we dropped below limit.
	total := ws.window.Total()
	if total < ws.limitTokens {
		freed := ws.limitTokens - total
		ws.window.ReplenishBurst(freed)
	}
}

// StatusAll returns the budget state for all registered workloads.
// Returns an empty map if no workloads are registered.
func (t *TrackerImpl) StatusAll(_ context.Context) map[string]WorkloadStatus {
	result := make(map[string]WorkloadStatus)
	t.windows.Range(func(key, value any) bool {
		ws := value.(*workloadState)
		result[key.(string)] = WorkloadStatus{
			LimitTokens:      ws.limitTokens,
			WindowTokensUsed: ws.window.Total(),
			BurstRemaining:   ws.window.BurstRemaining(),
			WindowSeconds:    ws.windowSeconds,
		}
		return true
	})
	return result
}

// Status returns the current budget status without modifying state.
// Returns ErrUnknownWorkload if the workload is not registered.
func (t *TrackerImpl) Status(_ context.Context, workloadID string) (BudgetStatus, error) {
	v, ok := t.windows.Load(workloadID)
	if !ok {
		return BudgetStatus{}, ErrUnknownWorkload
	}
	ws := v.(*workloadState)
	return BudgetStatus{
		WindowTokensUsed:  ws.window.Total(),
		WindowTokensLimit: ws.limitTokens,
		BurstRemaining:    ws.window.BurstRemaining(),
	}, nil
}
