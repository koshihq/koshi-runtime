package identity

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

// PodResolver resolves workload identity from environment variables injected
// by the Koshi mutating webhook at pod admission time. It does not make
// Kubernetes API calls at request time.
type PodResolver struct {
	namespace    string
	workloadKind string
	workloadName string
	podName      string
}

// NewPodResolver creates a PodResolver from the standard Koshi env vars.
// The webhook injects these as normalized values at admission time.
func NewPodResolver() *PodResolver {
	return &PodResolver{
		namespace:    strings.TrimSpace(os.Getenv("KOSHI_POD_NAMESPACE")),
		workloadKind: strings.TrimSpace(os.Getenv("KOSHI_WORKLOAD_KIND")),
		workloadName: strings.TrimSpace(os.Getenv("KOSHI_WORKLOAD_NAME")),
		podName:      strings.TrimSpace(os.Getenv("KOSHI_POD_NAME")),
	}
}

// NewPodResolverFromValues creates a PodResolver from explicit values.
// Useful for testing.
func NewPodResolverFromValues(namespace, workloadKind, workloadName, podName string) *PodResolver {
	return &PodResolver{
		namespace:    namespace,
		workloadKind: workloadKind,
		workloadName: workloadName,
		podName:      podName,
	}
}

// Resolve returns the workload identity derived from pod metadata.
// The request is unused — identity comes from env vars set at admission time.
func (r *PodResolver) Resolve(_ *http.Request) (WorkloadIdentity, error) {
	if r.namespace == "" {
		return WorkloadIdentity{}, ErrMissingIdentity
	}

	kind := r.workloadKind
	name := r.workloadName

	// Fall back to Pod/<podName> if workload kind/name are unset.
	if kind == "" || name == "" {
		if r.podName == "" {
			return WorkloadIdentity{}, ErrMissingIdentity
		}
		kind = "Pod"
		name = r.podName
	}

	return WorkloadIdentity{
		WorkloadID:   fmt.Sprintf("%s/%s/%s", r.namespace, kind, name),
		Namespace:    r.namespace,
		WorkloadKind: kind,
		WorkloadName: name,
	}, nil
}
