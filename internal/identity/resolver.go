package identity

import (
	"errors"
	"net/http"
	"strings"
)

// ErrMissingIdentity indicates the workload identity could not be resolved.
var ErrMissingIdentity = errors.New("missing workload identity")

// WorkloadIdentity represents a resolved workload.
type WorkloadIdentity struct {
	WorkloadID   string
	Namespace    string // Populated by PodResolver; empty for HeaderResolver.
	WorkloadKind string // Populated by PodResolver; empty for HeaderResolver.
	WorkloadName string // Populated by PodResolver; empty for HeaderResolver.
}

// Resolver resolves a workload identity from an incoming HTTP request.
type Resolver interface {
	Resolve(r *http.Request) (WorkloadIdentity, error)
}

// HeaderResolver resolves identity from a configurable HTTP header.
type HeaderResolver struct {
	HeaderKey string
}

// NewHeaderResolver creates a HeaderResolver for the given header key.
func NewHeaderResolver(headerKey string) *HeaderResolver {
	return &HeaderResolver{HeaderKey: headerKey}
}

// Resolve reads the configured header and returns the workload identity.
// Returns ErrMissingIdentity if the header is absent or empty.
func (r *HeaderResolver) Resolve(req *http.Request) (WorkloadIdentity, error) {
	val := strings.TrimSpace(req.Header.Get(r.HeaderKey))
	if val == "" {
		return WorkloadIdentity{}, ErrMissingIdentity
	}
	return WorkloadIdentity{WorkloadID: val}, nil
}
