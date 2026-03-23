package inject

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WebhookConfig holds configuration for the sidecar injector.
type WebhookConfig struct {
	SidecarImage string
	SidecarPort  int
	ConfigPath   string // ConfigMap mount path, e.g. /etc/koshi
	ScrapeAnnotations map[string]string // key→value for prometheus annotations
}

// HandleMutate processes an AdmissionReview and returns a response with
// sidecar injection patches.
func HandleMutate(cfg WebhookConfig, body []byte) (*admissionv1.AdmissionResponse, error) {
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		return nil, fmt.Errorf("unmarshal admission review: %w", err)
	}

	req := review.Request
	if req == nil {
		return nil, fmt.Errorf("admission review has no request")
	}

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return &admissionv1.AdmissionResponse{
			UID:     req.UID,
			Allowed: true,
		}, nil
	}

	// Check opt-out annotation.
	if pod.Annotations["runtime.getkoshi.ai/inject"] == "false" {
		return &admissionv1.AdmissionResponse{
			UID:     req.UID,
			Allowed: true,
		}, nil
	}

	// Idempotency: skip if sidecar already present.
	for _, c := range pod.Spec.Containers {
		if c.Name == "koshi-listener" {
			return &admissionv1.AdmissionResponse{
				UID:     req.UID,
				Allowed: true,
			}, nil
		}
	}

	patches := buildPatches(cfg, &pod, req.Namespace)
	patchBytes, err := MarshalPatches(patches)
	if err != nil {
		return nil, fmt.Errorf("marshal patches: %w", err)
	}

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		UID:       req.UID,
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
	}, nil
}

func buildPatches(cfg WebhookConfig, pod *corev1.Pod, namespace string) []JSONPatchOp {
	var patches []JSONPatchOp

	// Normalize workload identity from owner references.
	kind, name := NormalizeOwner(pod.OwnerReferences, pod.Labels, pod.Name)
	if pod.Name == "" && pod.GenerateName != "" {
		// For pods with generateName (most controller-managed pods), use generateName
		// minus trailing dash as a fallback pod name.
		fallbackName := pod.GenerateName
		if len(fallbackName) > 0 && fallbackName[len(fallbackName)-1] == '-' {
			fallbackName = fallbackName[:len(fallbackName)-1]
		}
		if kind == "Pod" {
			name = fallbackName
		}
	}

	podName := pod.Name
	if podName == "" {
		podName = pod.GenerateName
	}

	// Build sidecar container.
	sidecar := corev1.Container{
		Name:  "koshi-listener",
		Image: cfg.SidecarImage,
		Ports: []corev1.ContainerPort{
			{ContainerPort: int32(cfg.SidecarPort), Protocol: corev1.ProtocolTCP},
		},
		Env: []corev1.EnvVar{
			{Name: "KOSHI_CONFIG_PATH", Value: cfg.ConfigPath + "/config.yaml"},
			{Name: "KOSHI_POD_NAMESPACE", Value: namespace},
			{Name: "KOSHI_WORKLOAD_KIND", Value: kind},
			{Name: "KOSHI_WORKLOAD_NAME", Value: name},
			{Name: "KOSHI_POD_NAME", Value: podName},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "koshi-config", MountPath: cfg.ConfigPath, ReadOnly: true},
		},
	}

	// Add policy override if annotated.
	if policyOverride, ok := pod.Annotations["runtime.getkoshi.ai/policy"]; ok && policyOverride != "" {
		sidecar.Env = append(sidecar.Env, corev1.EnvVar{
			Name:  "KOSHI_POLICY_OVERRIDE",
			Value: policyOverride,
		})
	}

	// Add sidecar container.
	patches = append(patches, JSONPatchOp{
		Op:    "add",
		Path:  "/spec/containers/-",
		Value: sidecar,
	})

	// Inject base-URL env vars into app containers (only when not already set).
	providerEnvs := map[string]string{
		"OPENAI_BASE_URL":    fmt.Sprintf("http://localhost:%d", cfg.SidecarPort),
		"ANTHROPIC_BASE_URL": fmt.Sprintf("http://localhost:%d", cfg.SidecarPort),
	}

	for i, c := range pod.Spec.Containers {
		for envName, envValue := range providerEnvs {
			if !hasEnvVar(c.Env, envName) {
				patches = append(patches, JSONPatchOp{
					Op:   "add",
					Path: fmt.Sprintf("/spec/containers/%d/env/-", i),
					Value: corev1.EnvVar{
						Name:  envName,
						Value: envValue,
					},
				})
			}
		}
	}

	// Add config volume if not present.
	if !hasVolume(pod.Spec.Volumes, "koshi-config") {
		patches = append(patches, JSONPatchOp{
			Op:   "add",
			Path: "/spec/volumes/-",
			Value: corev1.Volume{
				Name: "koshi-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "koshi-config",
						},
					},
				},
			},
		})
	}

	// Add scrape annotations.
	if len(cfg.ScrapeAnnotations) > 0 {
		if pod.Annotations == nil {
			patches = append(patches, JSONPatchOp{
				Op:    "add",
				Path:  "/metadata/annotations",
				Value: map[string]string{},
			})
		}
		for key, val := range cfg.ScrapeAnnotations {
			if key == "prometheus.io/port" {
				val = strconv.Itoa(cfg.SidecarPort)
			}
			if key == "prometheus.io/path" {
				val = "/metrics"
			}
			if key == "prometheus.io/scrape" {
				val = "true"
			}
			patches = append(patches, JSONPatchOp{
				Op:    "add",
				Path:  "/metadata/annotations/" + escapeJSONPointer(key),
				Value: val,
			})
		}
	}

	return patches
}

func hasEnvVar(envs []corev1.EnvVar, name string) bool {
	for _, e := range envs {
		if e.Name == name {
			return true
		}
	}
	return false
}

func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// escapeJSONPointer escapes special characters in JSON Pointer tokens per RFC 6901.
func escapeJSONPointer(s string) string {
	// ~ must be escaped first, then /
	result := ""
	for _, c := range s {
		switch c {
		case '~':
			result += "~0"
		case '/':
			result += "~1"
		default:
			result += string(c)
		}
	}
	return result
}

// ServeWebhook returns an http.HandlerFunc that processes admission reviews.
func ServeWebhook(cfg WebhookConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		resp, err := HandleMutate(cfg, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		review := admissionv1.AdmissionReview{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "admission.k8s.io/v1",
				Kind:       "AdmissionReview",
			},
			Response: resp,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(review)
	}
}
