package inject

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNormalizeOwner(t *testing.T) {
	tests := []struct {
		name       string
		ownerRefs  []metav1.OwnerReference
		labels     map[string]string
		podName    string
		wantKind   string
		wantName   string
	}{
		{
			name:      "no owner falls back to Pod",
			ownerRefs: nil,
			podName:   "my-pod-abc123",
			wantKind:  "Pod",
			wantName:  "my-pod-abc123",
		},
		{
			name: "ReplicaSet with pod-template-hash normalizes to Deployment",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-app-7f9d8c5b6"},
			},
			labels:   map[string]string{"pod-template-hash": "7f9d8c5b6"},
			podName:  "my-app-7f9d8c5b6-xyz",
			wantKind: "Deployment",
			wantName: "my-app",
		},
		{
			name: "ReplicaSet without pod-template-hash stays as ReplicaSet",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-rs"},
			},
			labels:   map[string]string{},
			podName:  "my-rs-xyz",
			wantKind: "ReplicaSet",
			wantName: "my-rs",
		},
		{
			name: "ReplicaSet with hash that doesn't match suffix stays ReplicaSet",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-app-something"},
			},
			labels:   map[string]string{"pod-template-hash": "otherhash"},
			podName:  "pod-xyz",
			wantKind: "ReplicaSet",
			wantName: "my-app-something",
		},
		{
			name: "StatefulSet owner used directly",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "redis"},
			},
			podName:  "redis-0",
			wantKind: "StatefulSet",
			wantName: "redis",
		},
		{
			name: "DaemonSet owner used directly",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: "fluent-bit"},
			},
			podName:  "fluent-bit-abc",
			wantKind: "DaemonSet",
			wantName: "fluent-bit",
		},
		{
			name: "Job owner used directly",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "Job", Name: "batch-process-123"},
			},
			podName:  "batch-process-123-xyz",
			wantKind: "Job",
			wantName: "batch-process-123",
		},
		{
			name: "CronJob owner used directly",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "CronJob", Name: "nightly-sync"},
			},
			podName:  "nightly-sync-abc",
			wantKind: "CronJob",
			wantName: "nightly-sync",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, name := NormalizeOwner(tt.ownerRefs, tt.labels, tt.podName)
			if kind != tt.wantKind {
				t.Errorf("kind: got %q, want %q", kind, tt.wantKind)
			}
			if name != tt.wantName {
				t.Errorf("name: got %q, want %q", name, tt.wantName)
			}
		})
	}
}
