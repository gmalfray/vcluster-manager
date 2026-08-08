package kubernetes

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// GetNamespaceProtection reports whether the vcluster host namespace carries
// the protect-deletion annotation.
//
// The error return exists to separate "we looked and it's not there" from
// "we couldn't look" — the same distinction HostNamespaceState makes below,
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

// NamespaceState est ce qu'un seul Get dit du cycle de vie d'un namespace :
// s'il existe, depuis quand il est condamné (s'il l'est), et ce que
// l'apiserver écrit lui-même sur ce qui bloque sa terminaison.
//
// Conditions porte status.conditions tel quel — NamespaceFinalizersRemaining,
// NamespaceContentRemaining, NamespaceDeletionContentFailure, avec le
// finalizer ou la ressource concernée dans Message. C'est ce qui permet de
// NOMMER ce qui retient un namespace au lieu de deviner : deviner a fait dire
// « un finalizer tiers le retient » sur un namespace vieux de dix-sept
// secondes en recette, où rien ne le retenait du tout.
type NamespaceState struct {
	Exists            bool
	DeletionTimestamp time.Time
	Conditions        []corev1.NamespaceCondition
}

// HostNamespaceState lit le namespace hôte du vcluster en un seul Get.
//
// known distingue « j'ai regardé, il n'est pas là » de « je n'ai pas pu
// regarder » — même doctrine que GetNamespaceProtection juste au-dessus (qui a
// longtemps rendu false aussi bien pour une absence d'annotation que pour un
// namespace illisible, avant de gagner son propre retour d'erreur). Un
// namespace absent n'est pas une lecture ratée : c'est lui-même la réponse.
func (s *StatusClient) HostNamespaceState(ctx context.Context, name string) (state NamespaceState, known bool) {
	ns, err := s.client.Resource(namespaceGVR).Get(ctx, "vcluster-"+name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return NamespaceState{}, true
	}
	if err != nil {
		return NamespaceState{}, false
	}
	state.Exists = true
	if dt := ns.GetDeletionTimestamp(); dt != nil {
		state.DeletionTimestamp = dt.Time
	}
	state.Conditions = namespaceConditions(ns)
	return state, true
}

// namespaceConditions convertit status.conditions, lu comme n'importe quel
// autre champ d'un objet non typé, vers le type stable de l'API Kubernetes —
// pour porter les mêmes Type/Status/Reason/Message que `kubectl get ns -o
// yaml` montrerait, sans les redéfinir à côté.
func namespaceConditions(ns *unstructured.Unstructured) []corev1.NamespaceCondition {
	raw, found, err := unstructured.NestedSlice(ns.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}
	conds := make([]corev1.NamespaceCondition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		conds = append(conds, corev1.NamespaceCondition{
			Type:    corev1.NamespaceConditionType(nestedStringField(m, "type")),
			Status:  corev1.ConditionStatus(nestedStringField(m, "status")),
			Reason:  nestedStringField(m, "reason"),
			Message: nestedStringField(m, "message"),
		})
	}
	return conds
}

func nestedStringField(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}
