package proxy

import (
	"github.com/koshihq/koshi-runtime/internal/emit"
	"github.com/koshihq/koshi-runtime/internal/identity"
)

// Shadow decision values for listener mode events.
const (
	ShadowWouldReject   = "would_reject"
	ShadowWouldThrottle = "would_throttle"
	ShadowWouldKill     = "would_kill"
	ShadowAllow         = "allow"
)

// shadowEvent constructs a listener-mode shadow event with the stable field set.
func shadowEvent(
	ident identity.WorkloadIdentity,
	decisionShadow string,
	reasonCode string,
	providerName string,
	model string,
	estimatedTokens int64,
	actualTokens int64,
) emit.Event {
	attrs := map[string]any{
		"mode":             "listener",
		"decision_shadow":  decisionShadow,
		"reason_code":      reasonCode,
		"provider":         providerName,
		"model":            model,
		"estimated_tokens": estimatedTokens,
		"actual_tokens":    actualTokens,
	}

	// Add Kubernetes identity fields if populated (from PodResolver).
	if ident.Namespace != "" {
		attrs["namespace"] = ident.Namespace
	}
	if ident.WorkloadKind != "" {
		attrs["workload_kind"] = ident.WorkloadKind
	}
	if ident.WorkloadName != "" {
		attrs["workload_name"] = ident.WorkloadName
	}

	return emit.Event{
		Type:       "listener_shadow",
		WorkloadID: ident.WorkloadID,
		Severity:   "info",
		Attributes: attrs,
	}
}
