package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// serviceWithK8s builds a Service wired exactly like the operator's — a
// config and a Kubernetes client for "preprod", nothing else — backed by
// objs. Mirrors operatorWiredServiceWithK8s (vcluster_deletion_wiring_test.go)
// but lets the caller seed the fake client.
func serviceWithK8s(objs ...runtime.Object) *Service {
	var mu sync.RWMutex
	return New(Deps{
		Cfg:          &config.Config{},
		K8sClients:   map[string]*kubernetes.StatusClient{"preprod": kubernetes.NewTestStatusClient(objs...)},
		K8sClientsMu: &mu,
	})
}

func testPodObj(name, namespace string) *unstructured.Unstructured {
	pod := &unstructured.Unstructured{}
	pod.SetAPIVersion("v1")
	pod.SetKind("Pod")
	pod.SetName(name)
	pod.SetNamespace(namespace)
	return pod
}

func testArgoCDKustomizationObj(name, status string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata": map[string]interface{}{
			"name":      "argocd-" + name,
			"namespace": "vcluster-" + name,
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": status},
			},
		},
	}}
}

// --- podCount ------------------------------------------------------------

func TestObserveVCluster_ReportsPodCountWhenTheListSucceeds(t *testing.T) {
	s := serviceWithK8s(
		testPodObj("vcluster-demo-0", "vcluster-demo"),
		testPodObj("app-1", "vcluster-demo"),
	)

	obs := s.ObserveVCluster(context.Background(), "demo", "preprod")
	if !obs.PodCountKnown {
		t.Fatal("PodCountKnown=false alors que la liste a réussi")
	}
	if obs.PodCount != 2 {
		t.Fatalf("PodCount=%d, attendu 2", obs.PodCount)
	}
}

// Un vcluster réellement sans pod (arrêté, en cours de démarrage) doit
// pouvoir être vu à zéro — le zéro d'une lecture réussie n'est pas
// l'inconnue d'une lecture ratée.
func TestObserveVCluster_ZeroPodsIsAKnownFact(t *testing.T) {
	s := serviceWithK8s()

	obs := s.ObserveVCluster(context.Background(), "vide", "preprod")
	if !obs.PodCountKnown {
		t.Fatal("PodCountKnown=false sur un namespace vide et lisible")
	}
	if obs.PodCount != 0 {
		t.Fatalf("PodCount=%d, attendu 0", obs.PodCount)
	}
}

// Mutation clé de la mission : une liste de pods ratée doit rester
// distinguable d'un vcluster réellement vide. Si CountVClusterPods rendait
// (0, true) sur un échec, ce test tomberait — c'est le point.
func TestObserveVCluster_FailedPodListIsUnknownNotZero(t *testing.T) {
	var mu sync.RWMutex
	refus := apierrors.NewInternalError(errors.New("etcd unavailable"))
	k8s := kubernetes.NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			return true, nil, refus
		}
		return false, nil, nil
	}, testPodObj("vcluster-demo-0", "vcluster-demo"))
	s := New(Deps{
		Cfg:          &config.Config{},
		K8sClients:   map[string]*kubernetes.StatusClient{"preprod": k8s},
		K8sClientsMu: &mu,
	})

	obs := s.ObserveVCluster(context.Background(), "demo", "preprod")
	if obs.PodCountKnown {
		t.Fatal("PodCountKnown=true alors que la liste des pods a échoué")
	}
}

// --- ArgoCD Kustomization --------------------------------------------------

func TestObserveVCluster_ReportsTheArgoCDKustomizationState(t *testing.T) {
	s := serviceWithK8s(testArgoCDKustomizationObj("demo", "True"))

	obs := s.ObserveVCluster(context.Background(), "demo", "preprod")
	if obs.ArgoCDKustomization != "Ready" {
		t.Fatalf("ArgoCDKustomization=%q, attendu Ready", obs.ArgoCDKustomization)
	}
}

// Rien de commité pour ce vcluster (ArgoCD jamais demandé, ou pas encore
// appliqué par Flux) : Unknown, pas une erreur.
func TestObserveVCluster_MissingArgoCDKustomizationIsUnknown(t *testing.T) {
	s := serviceWithK8s()

	obs := s.ObserveVCluster(context.Background(), "sans-argocd", "preprod")
	if obs.ArgoCDKustomization != "Unknown" {
		t.Fatalf("ArgoCDKustomization=%q, attendu Unknown", obs.ArgoCDKustomization)
	}
}
