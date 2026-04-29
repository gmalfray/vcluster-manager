package kubernetes

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// HasRancherAgents checks whether Rancher agents are running inside a vcluster.
// vcluster syncs pods from inside the virtual cluster to the host namespace (vcluster-{name})
// with a label indicating their original namespace. We look for pods from cattle-system,
// which is the namespace Rancher agents run in — this detects pairings regardless of
// how the cluster was named in Rancher.
func (s *StatusClient) HasRancherAgents(ctx context.Context, name string) bool {
	namespace := "vcluster-" + name
	podGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

	// vcluster labels synced pods with their original namespace.
	// Try both label keys used by different vcluster versions.
	for _, labelSelector := range []string{
		"vcluster.loft.sh/namespace=cattle-system",
		"vcluster.loft.sh/object-namespace=cattle-system",
	} {
		list, err := s.client.Resource(podGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
			Limit:         1,
		})
		if err == nil && len(list.Items) > 0 {
			return true
		}
	}
	return false
}
