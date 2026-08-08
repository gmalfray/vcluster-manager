package kubernetes

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// --- CountVClusterPods -------------------------------------------------

// Trois pods dans le namespace hôte, zéro ailleurs : seuls ceux du vcluster
// visé comptent.
func TestCountVClusterPodsCountsOnlyTheHostNamespace(t *testing.T) {
	s := NewTestStatusClient(
		newPodObj("vcluster-demo-0", "vcluster-demo", nil),
		newPodObj("vcluster-demo-etcd-0", "vcluster-demo", nil),
		newPodObj("app-1", "vcluster-demo", nil),
		newPodObj("autre-namespace-pod", "vcluster-autre", nil),
	)

	count, known := s.CountVClusterPods(context.Background(), "demo")
	if !known {
		t.Fatal("known=false alors que la liste a réussi")
	}
	if count != 3 {
		t.Fatalf("count=%d, attendu 3 (le pod de vcluster-autre ne doit pas compter)", count)
	}
}

// Zéro pod est un fait qu'on a lu, pas une absence de lecture : known doit
// rester true.
func TestCountVClusterPodsZeroPodsIsKnown(t *testing.T) {
	s := NewTestStatusClient()

	count, known := s.CountVClusterPods(context.Background(), "vide")
	if !known {
		t.Fatal("known=false sur un namespace vide et lisible : « zéro pod » est un fait, pas une inconnue")
	}
	if count != 0 {
		t.Fatalf("count=%d, attendu 0", count)
	}
}

// Une liste ratée doit rendre known=false, jamais un count à zéro qui se
// lirait « le vcluster ne fait tourner aucun pod » sur un hoquet d'API.
func TestCountVClusterPodsFailedListIsUnknownNotZero(t *testing.T) {
	refus := apierrors.NewInternalError(errors.New("etcd unavailable"))
	s := NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			return true, nil, refus
		}
		return false, nil, nil
	}, newPodObj("vcluster-demo-0", "vcluster-demo", nil))

	count, known := s.CountVClusterPods(context.Background(), "demo")
	if known {
		t.Fatal("known=true alors que la liste des pods a échoué : le nombre affiché ne serait pas fiable")
	}
	if count != 0 {
		t.Fatalf("count=%d sur une lecture ratée, attendu 0 (la valeur ne doit de toute façon pas être lue tant que known=false)", count)
	}
}

// --- GetArgoCDKustomizationStatus ---------------------------------------

// argocdKustomizationObj construit la Kustomization argocd-<name>, dans le
// namespace hôte du vcluster — pas flux-system, où vit la Kustomization
// tenant que GetVClusterStatus lit déjà.
func argocdKustomizationObj(name, status, reason string) *unstructured.Unstructured {
	cond := map[string]interface{}{"type": "Ready", "status": status}
	if reason != "" {
		cond["reason"] = reason
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata": map[string]interface{}{
			"name":      "argocd-" + name,
			"namespace": "vcluster-" + name,
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{cond},
		},
	}}
}

func TestGetArgoCDKustomizationStatusReadsTheReadyCondition(t *testing.T) {
	s := NewTestStatusClient(argocdKustomizationObj("demo", "True", ""))

	if got := s.GetArgoCDKustomizationStatus(context.Background(), "demo"); got != "Ready" {
		t.Fatalf("état = %q, attendu Ready", got)
	}
}

// Un échec explicite se distingue d'un « on ne sait pas » : le reason Flux
// (UpgradeFailed, Progressing…) doit traverser tel quel.
func TestGetArgoCDKustomizationStatusReportsAnExplicitFailure(t *testing.T) {
	s := NewTestStatusClient(argocdKustomizationObj("demo", "False", "BuildFailed"))

	if got := s.GetArgoCDKustomizationStatus(context.Background(), "demo"); got != "BuildFailed" {
		t.Fatalf("état = %q, attendu BuildFailed", got)
	}
}

// Pas encore créée (Flux n'a pas encore appliqué le graphe tenant) : Unknown,
// pas une erreur — même vocabulaire que le HelmRelease et la Kustomization
// tenant.
func TestGetArgoCDKustomizationStatusNotFoundIsUnknown(t *testing.T) {
	s := NewTestStatusClient()

	if got := s.GetArgoCDKustomizationStatus(context.Background(), "jamais-cree"); got != "Unknown" {
		t.Fatalf("état = %q, attendu Unknown", got)
	}
}

// Le nom ET le namespace comptent : une Kustomization argocd-<name> qui vit
// ailleurs (mauvais vcluster, ou dans flux-system comme la tenant) ne doit
// pas être lue à sa place.
func TestGetArgoCDKustomizationStatusIsScopedToItsOwnNamespace(t *testing.T) {
	s := NewTestStatusClient(argocdKustomizationObj("autre-vcluster", "True", ""))

	if got := s.GetArgoCDKustomizationStatus(context.Background(), "demo"); got != "Unknown" {
		t.Fatalf("état = %q, attendu Unknown : la Kustomization d'un autre vcluster a été lue", got)
	}
}
