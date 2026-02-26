package identity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHeaderResolver_Present(t *testing.T) {
	r := NewHeaderResolver("x-genops-workload-id")
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("x-genops-workload-id", "svc-a")

	id, err := r.Resolve(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.WorkloadID != "svc-a" {
		t.Errorf("expected svc-a, got %s", id.WorkloadID)
	}
}

func TestHeaderResolver_Missing(t *testing.T) {
	r := NewHeaderResolver("x-genops-workload-id")
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	_, err := r.Resolve(req)
	if !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("expected ErrMissingIdentity, got: %v", err)
	}
}

func TestHeaderResolver_Empty(t *testing.T) {
	r := NewHeaderResolver("x-genops-workload-id")
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("x-genops-workload-id", "")

	_, err := r.Resolve(req)
	if !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("expected ErrMissingIdentity for empty header, got: %v", err)
	}
}

func TestHeaderResolver_Whitespace(t *testing.T) {
	r := NewHeaderResolver("x-genops-workload-id")
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("x-genops-workload-id", "   ")

	_, err := r.Resolve(req)
	if !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("expected ErrMissingIdentity for whitespace-only header, got: %v", err)
	}
}

func TestHeaderResolver_TrimWhitespace(t *testing.T) {
	r := NewHeaderResolver("x-genops-workload-id")
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("x-genops-workload-id", "  svc-a  ")

	id, err := r.Resolve(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.WorkloadID != "svc-a" {
		t.Errorf("expected trimmed svc-a, got %q", id.WorkloadID)
	}
}
