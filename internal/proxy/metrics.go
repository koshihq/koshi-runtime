package proxy

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// workload_id is config-derived and bounded. It represents the enforcement
// boundary in v1. It must not be derived from request input. If any path
// allows dynamic values, sanitize or constrain before using as a label.

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "koshi",
		Name:      "requests_total",
		Help:      "Total requests by workload, policy, and decision.",
	}, []string{"workload_id", "policy_id", "decision"})

	tokensUsedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "koshi",
		Name:      "tokens_used_total",
		Help:      "Total tokens by workload, policy, and lifecycle phase.",
	}, []string{"workload_id", "policy_id", "phase"})

	enforcementDecisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "koshi",
		Name:      "enforcement_decisions_total",
		Help:      "Total enforcement decisions by action and policy.",
	}, []string{"action", "policy_id"})

	enforcementLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "koshi",
		Name:      "enforcement_latency_seconds",
		Help:      "Time spent in enforcement logic (policy eval + guard checks).",
		Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
	})
)
