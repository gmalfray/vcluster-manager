package kubernetes

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetNamespaceProtection returns true if the vcluster host namespace has the protect-deletion annotation.
func (s *StatusClient) GetNamespaceProtection(ctx context.Context, name string) bool {
	ns, err := s.client.Resource(namespaceGVR).Get(ctx, "vcluster-"+name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return ns.GetAnnotations()["protect-deletion"] == "true"
}

// SetNamespaceProtection adds or removes the protect-deletion annotation on the vcluster host namespace.
func (s *StatusClient) SetNamespaceProtection(ctx context.Context, name string, enabled bool) error {
	ns, err := s.client.Resource(namespaceGVR).Get(ctx, "vcluster-"+name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting namespace vcluster-%s: %w", name, err)
	}
	annotations := ns.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if enabled {
		annotations["protect-deletion"] = "true"
	} else {
		delete(annotations, "protect-deletion")
	}
	ns.SetAnnotations(annotations)
	_, err = s.client.Resource(namespaceGVR).Update(ctx, ns, metav1.UpdateOptions{})
	return err
}
