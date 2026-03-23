package inject

import (
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func testConfig() WebhookConfig {
	return WebhookConfig{
		SidecarImage: "koshi:latest",
		SidecarPort:  15080,
		ConfigPath:   "/etc/koshi",
		ScrapeAnnotations: map[string]string{
			"prometheus.io/scrape": "true",
			"prometheus.io/port":   "15080",
			"prometheus.io/path":   "/metrics",
		},
	}
}

func makeReview(pod *corev1.Pod, namespace string) []byte {
	podBytes, _ := json.Marshal(pod)
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Namespace: namespace,
			Object:    runtime.RawExtension{Raw: podBytes},
		},
	}
	b, _ := json.Marshal(review)
	return b
}

func TestWebhook_InjectsSidecar(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-app-abc123"},
			},
			Labels: map[string]string{"pod-template-hash": "abc123"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest", Env: []corev1.EnvVar{}},
			},
		},
	}

	resp, err := HandleMutate(testConfig(), makeReview(pod, "prod"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Allowed {
		t.Fatal("expected allowed")
	}
	if resp.Patch == nil {
		t.Fatal("expected patches")
	}

	var patches []JSONPatchOp
	json.Unmarshal(resp.Patch, &patches)

	// Should have patches for: sidecar container, env vars (OPENAI_BASE_URL, ANTHROPIC_BASE_URL),
	// config volume, scrape annotations.
	if len(patches) < 3 {
		t.Errorf("expected at least 3 patches, got %d", len(patches))
	}

	// Find the sidecar container patch.
	found := false
	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			found = true
			// Verify the container has the right env vars.
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			if container.Name != "koshi-listener" {
				t.Errorf("expected container name koshi-listener, got %s", container.Name)
			}
			// Check KOSHI_WORKLOAD_KIND is set to Deployment (normalized from ReplicaSet).
			checkEnv(t, container.Env, "KOSHI_WORKLOAD_KIND", "Deployment")
			checkEnv(t, container.Env, "KOSHI_WORKLOAD_NAME", "my-app")
			checkEnv(t, container.Env, "KOSHI_POD_NAMESPACE", "prod")
		}
	}
	if !found {
		t.Error("expected sidecar container patch")
	}
}

func TestWebhook_OptOut(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-pod",
			Annotations: map[string]string{"koshi.io/inject": "false"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	resp, err := HandleMutate(testConfig(), makeReview(pod, "prod"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Allowed {
		t.Fatal("expected allowed")
	}
	if resp.Patch != nil {
		t.Error("expected no patches for opt-out pod")
	}
}

func TestWebhook_Idempotent(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "koshi-listener"}, // Already injected
			},
		},
	}

	resp, err := HandleMutate(testConfig(), makeReview(pod, "prod"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Patch != nil {
		t.Error("expected no patches for pod with existing sidecar")
	}
}

func TestWebhook_PolicyOverride(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-pod",
			Annotations: map[string]string{"koshi.io/policy": "custom-policy"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{}}},
		},
	}

	resp, err := HandleMutate(testConfig(), makeReview(pod, "prod"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []JSONPatchOp
	json.Unmarshal(resp.Patch, &patches)

	// Find sidecar container and check for KOSHI_POLICY_OVERRIDE.
	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			checkEnv(t, container.Env, "KOSHI_POLICY_OVERRIDE", "custom-policy")
			return
		}
	}
	t.Error("expected sidecar container patch with policy override")
}

func TestWebhook_NoClobberExistingEnv(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Env: []corev1.EnvVar{
						{Name: "OPENAI_BASE_URL", Value: "https://custom-proxy.example.com"},
					},
				},
			},
		},
	}

	resp, err := HandleMutate(testConfig(), makeReview(pod, "prod"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []JSONPatchOp
	json.Unmarshal(resp.Patch, &patches)

	// Should NOT have a patch adding OPENAI_BASE_URL to the app container.
	for _, p := range patches {
		if p.Path == "/spec/containers/0/env/-" {
			envBytes, _ := json.Marshal(p.Value)
			var env corev1.EnvVar
			json.Unmarshal(envBytes, &env)
			if env.Name == "OPENAI_BASE_URL" {
				t.Error("should not clobber existing OPENAI_BASE_URL")
			}
		}
	}

	// ANTHROPIC_BASE_URL should still be injected since it's not set.
	foundAnthropic := false
	for _, p := range patches {
		if p.Path == "/spec/containers/0/env/-" {
			envBytes, _ := json.Marshal(p.Value)
			var env corev1.EnvVar
			json.Unmarshal(envBytes, &env)
			if env.Name == "ANTHROPIC_BASE_URL" {
				foundAnthropic = true
			}
		}
	}
	if !foundAnthropic {
		t.Error("expected ANTHROPIC_BASE_URL to be injected")
	}
}

func checkEnv(t *testing.T, envs []corev1.EnvVar, name, expectedValue string) {
	t.Helper()
	for _, e := range envs {
		if e.Name == name {
			if e.Value != expectedValue {
				t.Errorf("env %s: expected %q, got %q", name, expectedValue, e.Value)
			}
			return
		}
	}
	t.Errorf("env %s not found", name)
}
