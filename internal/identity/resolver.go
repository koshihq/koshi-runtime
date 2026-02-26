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
	WorkloadID string
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
