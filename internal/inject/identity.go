package inject

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NormalizeOwner derives a stable workload kind and name from pod metadata.
//
// Rules:
//   - StatefulSet, DaemonSet, Job, CronJob → use owner directly
//   - ReplicaSet with pod-template-hash label → Deployment/<rs name with -<hash> stripped>
//   - ReplicaSet without pod-template-hash → ReplicaSet/<name>
//   - No owner → Pod/<podName>
func NormalizeOwner(ownerRefs []metav1.OwnerReference, labels map[string]string, podName string) (kind, name string) {
	if len(ownerRefs) == 0 {
		return "Pod", podName
	}

	owner := ownerRefs[0]

	switch owner.Kind {
	case "StatefulSet", "DaemonSet", "Job", "CronJob":
		return owner.Kind, owner.Name

	case "ReplicaSet":
		hash, ok := labels["pod-template-hash"]
		if ok && hash != "" {
			suffix := "-" + hash
			if strings.HasSuffix(owner.Name, suffix) {
				deployName := strings.TrimSuffix(owner.Name, suffix)
				return "Deployment", deployName
			}
		}
		return "ReplicaSet", owner.Name

	default:
		// Unknown owner kind — use it directly.
		return owner.Kind, owner.Name
	}
}
