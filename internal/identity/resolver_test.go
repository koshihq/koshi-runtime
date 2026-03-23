package identity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- PodResolver tests ---

func TestPodResolver_FullIdentity(t *testing.T) {
	r := NewPodResolverFromValues("prod", "Deployment", "my-app", "my-app-abc123")
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	id, err := r.Resolve(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.WorkloadID != "prod/Deployment/my-app" {
		t.Errorf("expected prod/Deployment/my-app, got %s", id.WorkloadID)
	}
	if id.Namespace != "prod" {
		t.Errorf("expected namespace prod, got %s", id.Namespace)
	}
	if id.WorkloadKind != "Deployment" {
		t.Errorf("expected kind Deployment, got %s", id.WorkloadKind)
	}
	if id.WorkloadName != "my-app" {
		t.Errorf("expected name my-app, got %s", id.WorkloadName)
	}
}

func TestPodResolver_FallbackToPod(t *testing.T) {
	r := NewPodResolverFromValues("staging", "", "", "my-pod-xyz")
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	id, err := r.Resolve(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.WorkloadID != "staging/Pod/my-pod-xyz" {
		t.Errorf("expected staging/Pod/my-pod-xyz, got %s", id.WorkloadID)
	}
	if id.WorkloadKind != "Pod" {
		t.Errorf("expected kind Pod, got %s", id.WorkloadKind)
	}
}

func TestPodResolver_MissingNamespace(t *testing.T) {
	r := NewPodResolverFromValues("", "Deployment", "my-app", "my-pod")
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	_, err := r.Resolve(req)
	if !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("expected ErrMissingIdentity, got: %v", err)
	}
}

func TestPodResolver_MissingEverything(t *testing.T) {
	r := NewPodResolverFromValues("default", "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	_, err := r.Resolve(req)
	if !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("expected ErrMissingIdentity when no kind/name and no podName, got: %v", err)
	}
}

func TestPodResolver_PartialWorkload_KindOnly(t *testing.T) {
	r := NewPodResolverFromValues("ns", "Deployment", "", "my-pod")
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	id, err := r.Resolve(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Falls back to Pod because name is empty
	if id.WorkloadID != "ns/Pod/my-pod" {
		t.Errorf("expected ns/Pod/my-pod, got %s", id.WorkloadID)
	}
}

// --- HeaderResolver tests ---

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
