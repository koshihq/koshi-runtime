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
			Annotations: map[string]string{"runtime.getkoshi.ai/inject": "false"},
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
			Annotations: map[string]string{"runtime.getkoshi.ai/policy": "custom-policy"},
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

func TestWebhook_NilEnv(t *testing.T) {
	// Pod with no env — JSON patch must create env array before appending.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bare-pod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "bare-app-abc123"},
			},
			Labels: map[string]string{"pod-template-hash": "abc123"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
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

	var patches []JSONPatchOp
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	// Verify env array init appears before env appends.
	envInitIdx := -1
	envAppendIdx := -1

	for i, p := range patches {
		switch {
		case p.Op == "add" && p.Path == "/spec/containers/0/env":
			envInitIdx = i
		case p.Op == "add" && p.Path == "/spec/containers/0/env/-" && envAppendIdx == -1:
			envAppendIdx = i
		}
	}

	if envInitIdx == -1 {
		t.Error("expected env array init patch (/spec/containers/0/env)")
	}
	if envAppendIdx == -1 {
		t.Error("expected env append patch (/spec/containers/0/env/-)")
	}
	if envInitIdx != -1 && envAppendIdx != -1 && envInitIdx >= envAppendIdx {
		t.Error("env array init must come before env append")
	}

	// No volume patches should be present (sidecar no longer mounts a ConfigMap).
	for _, p := range patches {
		if p.Path == "/spec/volumes" || p.Path == "/spec/volumes/-" {
			t.Errorf("unexpected volume patch: %s %s", p.Op, p.Path)
		}
	}
}

func TestWebhook_SidecarPullPolicy(t *testing.T) {
	cfg := testConfig()
	cfg.SidecarPullPolicy = corev1.PullIfNotPresent

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest", Env: []corev1.EnvVar{}},
			},
		},
	}

	resp, err := HandleMutate(cfg, makeReview(pod, "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []JSONPatchOp
	json.Unmarshal(resp.Patch, &patches)

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			if container.ImagePullPolicy != corev1.PullIfNotPresent {
				t.Errorf("expected imagePullPolicy IfNotPresent, got %q", container.ImagePullPolicy)
			}
			return
		}
	}
	t.Error("expected sidecar container patch")
}

func TestWebhook_ListenAddrEnv(t *testing.T) {
	cfg := testConfig()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest", Env: []corev1.EnvVar{}},
			},
		},
	}

	resp, err := HandleMutate(cfg, makeReview(pod, "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []JSONPatchOp
	json.Unmarshal(resp.Patch, &patches)

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			checkEnv(t, container.Env, "KOSHI_LISTEN_ADDR", ":15080")
			return
		}
	}
	t.Error("expected sidecar container patch")
}

func TestWebhook_CustomPortListenAddr(t *testing.T) {
	cfg := testConfig()
	cfg.SidecarPort = 16080
	cfg.ScrapeAnnotations["prometheus.io/port"] = "16080"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest", Env: []corev1.EnvVar{}},
			},
		},
	}

	resp, err := HandleMutate(cfg, makeReview(pod, "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []JSONPatchOp
	json.Unmarshal(resp.Patch, &patches)

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			checkEnv(t, container.Env, "KOSHI_LISTEN_ADDR", ":16080")
			if len(container.Ports) == 0 || container.Ports[0].ContainerPort != 16080 {
				t.Errorf("expected containerPort 16080, got %v", container.Ports)
			}
			return
		}
	}
	t.Error("expected sidecar container patch")
}

func TestWebhook_EnforcementModeAnnotation(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-pod",
			Annotations: map[string]string{"runtime.getkoshi.ai/mode": "enforcement"},
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

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			checkEnv(t, container.Env, "KOSHI_MODE", "enforcement")
			return
		}
	}
	t.Error("expected sidecar container patch with KOSHI_MODE")
}

func TestWebhook_ListenerModeAnnotation_NoKoshiMode(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-pod",
			Annotations: map[string]string{"runtime.getkoshi.ai/mode": "listener"},
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

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			for _, e := range container.Env {
				if e.Name == "KOSHI_MODE" {
					t.Error("KOSHI_MODE should not be injected for listener mode annotation")
				}
			}
			return
		}
	}
	t.Error("expected sidecar container patch")
}

func TestWebhook_NoModeAnnotation_NoKoshiMode(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod"},
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

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			for _, e := range container.Env {
				if e.Name == "KOSHI_MODE" {
					t.Error("KOSHI_MODE should not be injected when no mode annotation present")
				}
			}
			return
		}
	}
	t.Error("expected sidecar container patch")
}

func TestWebhook_ModeAndPolicyAnnotationsTogether(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
			Annotations: map[string]string{
				"runtime.getkoshi.ai/mode":   "enforcement",
				"runtime.getkoshi.ai/policy": "sidecar-strict",
			},
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

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			checkEnv(t, container.Env, "KOSHI_MODE", "enforcement")
			checkEnv(t, container.Env, "KOSHI_POLICY_OVERRIDE", "sidecar-strict")
			return
		}
	}
	t.Error("expected sidecar container patch with both KOSHI_MODE and KOSHI_POLICY_OVERRIDE")
}

func TestWebhook_UnknownModeAnnotation_NoKoshiMode(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-pod",
			Annotations: map[string]string{"runtime.getkoshi.ai/mode": "bogus"},
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

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			for _, e := range container.Env {
				if e.Name == "KOSHI_MODE" {
					t.Error("KOSHI_MODE should not be injected for unrecognized mode annotation")
				}
			}
			return
		}
	}
	t.Error("expected sidecar container patch")
}

func TestWebhook_ConfigmapAnnotation_InjectsVolumeAndMount(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
			Annotations: map[string]string{
				"runtime.getkoshi.ai/configmap": "my-custom-config",
				"runtime.getkoshi.ai/policy":    "custom-policy",
			},
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

	// Verify sidecar has KOSHI_CONFIG_PATH env var and volumeMount.
	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			checkEnv(t, container.Env, "KOSHI_CONFIG_PATH", "/etc/koshi-sidecar/config.yaml")
			checkEnv(t, container.Env, "KOSHI_POLICY_OVERRIDE", "custom-policy")
			if len(container.VolumeMounts) != 1 {
				t.Fatalf("expected 1 volumeMount, got %d", len(container.VolumeMounts))
			}
			vm := container.VolumeMounts[0]
			if vm.Name != "koshi-sidecar-config" {
				t.Errorf("expected volumeMount name koshi-sidecar-config, got %q", vm.Name)
			}
			if vm.MountPath != "/etc/koshi-sidecar" {
				t.Errorf("expected mountPath /etc/koshi-sidecar, got %q", vm.MountPath)
			}
			if !vm.ReadOnly {
				t.Error("expected volumeMount to be read-only")
			}
			break
		}
	}

	// Verify volume init and append patches.
	foundVolInit := false
	foundVolAppend := false
	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/volumes" {
			foundVolInit = true
		}
		if p.Op == "add" && p.Path == "/spec/volumes/-" {
			foundVolAppend = true
			volBytes, _ := json.Marshal(p.Value)
			var vol corev1.Volume
			json.Unmarshal(volBytes, &vol)
			if vol.Name != "koshi-sidecar-config" {
				t.Errorf("expected volume name koshi-sidecar-config, got %q", vol.Name)
			}
			if vol.ConfigMap == nil || vol.ConfigMap.Name != "my-custom-config" {
				t.Errorf("expected configMap name my-custom-config, got %+v", vol.ConfigMap)
			}
		}
	}
	if !foundVolInit {
		t.Error("expected volume init patch (/spec/volumes)")
	}
	if !foundVolAppend {
		t.Error("expected volume append patch (/spec/volumes/-)")
	}
}

func TestWebhook_NoConfigmapAnnotation_NoVolumes(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod"},
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

	for _, p := range patches {
		if p.Path == "/spec/volumes" || p.Path == "/spec/volumes/-" {
			t.Errorf("unexpected volume patch when configmap annotation absent: %s %s", p.Op, p.Path)
		}
	}

	// Sidecar should not have KOSHI_CONFIG_PATH.
	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			for _, e := range container.Env {
				if e.Name == "KOSHI_CONFIG_PATH" {
					t.Error("KOSHI_CONFIG_PATH should not be set when configmap annotation is absent")
				}
			}
			break
		}
	}
}

func TestWebhook_ConfigmapAndModeAndPolicy_AllThree(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
			Annotations: map[string]string{
				"runtime.getkoshi.ai/mode":      "enforcement",
				"runtime.getkoshi.ai/policy":    "my-custom-policy",
				"runtime.getkoshi.ai/configmap": "my-config",
			},
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

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/containers/-" {
			containerBytes, _ := json.Marshal(p.Value)
			var container corev1.Container
			json.Unmarshal(containerBytes, &container)
			checkEnv(t, container.Env, "KOSHI_MODE", "enforcement")
			checkEnv(t, container.Env, "KOSHI_POLICY_OVERRIDE", "my-custom-policy")
			checkEnv(t, container.Env, "KOSHI_CONFIG_PATH", "/etc/koshi-sidecar/config.yaml")
			if len(container.VolumeMounts) != 1 {
				t.Errorf("expected 1 volumeMount, got %d", len(container.VolumeMounts))
			}
			return
		}
	}
	t.Error("expected sidecar container patch")
}

func TestWebhook_ConfigmapWithoutPolicy_StillInjects(t *testing.T) {
	// Webhook injects volume/mount regardless — policy requirement is enforced at
	// sidecar startup, not admission time.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
			Annotations: map[string]string{
				"runtime.getkoshi.ai/configmap": "my-config",
			},
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

	foundVolume := false
	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/volumes/-" {
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Error("expected volume patch even without policy annotation")
	}
}

func TestWebhook_ConfigmapWithExistingVolumes_NoInit(t *testing.T) {
	// Pod already has volumes — should NOT get /spec/volumes init patch.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
			Annotations: map[string]string{
				"runtime.getkoshi.ai/configmap": "my-config",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{}}},
			Volumes: []corev1.Volume{
				{Name: "existing-vol"},
			},
		},
	}

	resp, err := HandleMutate(testConfig(), makeReview(pod, "prod"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []JSONPatchOp
	json.Unmarshal(resp.Patch, &patches)

	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/volumes" {
			t.Error("should not init volumes array when pod already has volumes")
		}
	}

	// Should still have the append patch.
	foundAppend := false
	for _, p := range patches {
		if p.Op == "add" && p.Path == "/spec/volumes/-" {
			foundAppend = true
		}
	}
	if !foundAppend {
		t.Error("expected volume append patch")
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
