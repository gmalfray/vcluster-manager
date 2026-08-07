package kubernetes

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetNamespaceProtection reports whether the vcluster host namespace carries
// the protect-deletion annotation.
//
// The error return exists to separate "we looked and it's not there" from
// "we couldn't look" — the same distinction HostNamespaceExists makes below,
// for the same reason: a caller that treats a failed read as "not protected"
// silently drops a safeguard on an API hiccup instead of on an actual
// decision. A missing namespace is not a failed read, though: it is itself
// the answer (no namespace, no annotation on it), so it comes back as
// (false, nil) rather than an error.
func (s *StatusClient) GetNamespaceProtection(ctx context.Context, name string) (bool, error) {
	ns, err := s.client.Resource(namespaceGVR).Get(ctx, "vcluster-"+name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ns.GetAnnotations()["protect-deletion"] == "true", nil
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

// HostNamespaceExists dit si le namespace hôte du vcluster est là.
//
// Trois réponses et non deux, délibérément : « il existe », « il n'existe pas »,
// et « je n'ai pas pu savoir ». Même doctrine que GetNamespaceProtection
// juste au-dessus (qui a longtemps rendu false aussi bien pour une absence
// d'annotation que pour un namespace illisible, avant de gagner son propre
// retour d'erreur) — et cette fonction-ci sert à décider s'il faut exiger une
// sauvegarde avant destruction, donc confondre les deux ferait sauter le
// filet sur un hoquet d'API.
func (s *StatusClient) HostNamespaceExists(ctx context.Context, name string) (exists, known bool) {
	_, err := s.client.Resource(namespaceGVR).Get(ctx, "vcluster-"+name, metav1.GetOptions{})
	if err == nil {
		return true, true
	}
	if apierrors.IsNotFound(err) {
		return false, true
	}
	return false, false
}
