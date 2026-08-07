package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// unstructuredNamespace builds the vcluster host namespace as the fake
// dynamic client expects it — same shape kubernetes.GetNamespaceProtection
// reads in production.
func unstructuredNamespace(name string, annotations map[string]string) *unstructured.Unstructured {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName("vcluster-" + name)
	if annotations != nil {
		ns.SetAnnotations(annotations)
	}
	return ns
}

// newTestService builds a Service with no Kubernetes clients — enough to
// exercise the transport-agnostic business rules (RBAC, env defaulting, k8s
// availability) without an HTTP request or a real cluster.
func newTestService() *Service {
	var mu sync.RWMutex
	return New(Deps{K8sClients: map[string]*kubernetes.StatusClient{}, K8sClientsMu: &mu})
}

func TestSetProtection_ForbiddenForNonAdmin(t *testing.T) {
	s := newTestService()
	_, err := s.SetProtection(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, "demo", "preprod", true)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestSetProtection_K8sUnavailable(t *testing.T) {
	s := newTestService()
	_, err := s.SetProtection(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod", true)
	if !errors.Is(err, ErrK8sUnavailable) {
		t.Fatalf("expected ErrK8sUnavailable when no client is configured, got %v", err)
	}
}

func TestSetProtection_K8sUnavailable_EnvDefaultsToPreprod(t *testing.T) {
	// A client registered under "preprod" but no explicit env passed still
	// gets picked up: env "" must default to "preprod" before the lookup.
	var mu sync.RWMutex
	s := New(Deps{K8sClients: map[string]*kubernetes.StatusClient{}, K8sClientsMu: &mu})
	_, err := s.SetProtection(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "", true)
	if !errors.Is(err, ErrK8sUnavailable) {
		t.Fatalf("expected ErrK8sUnavailable, got %v", err)
	}
}

func TestGetProtection_UnavailableWhenNoClient(t *testing.T) {
	s := newTestService()
	st := s.GetProtection(context.Background(), "demo", "")
	if st.Available {
		t.Fatalf("expected Available=false when no client is configured")
	}
	if st.Name != "demo" {
		t.Fatalf("expected Name to be echoed back, got %q", st.Name)
	}
	if st.Env != "preprod" {
		t.Fatalf("expected empty env to default to preprod, got %q", st.Env)
	}
}

func TestGetProtection_EchoesRequestedEnv(t *testing.T) {
	s := newTestService()
	st := s.GetProtection(context.Background(), "demo", "prod")
	if st.Env != "prod" {
		t.Fatalf("expected Env=prod to be preserved, got %q", st.Env)
	}
}

func serviceWithK8sClient(env string, k8s *kubernetes.StatusClient) *Service {
	var mu sync.RWMutex
	return New(Deps{K8sClients: map[string]*kubernetes.StatusClient{env: k8s}, K8sClientsMu: &mu})
}

func TestGetProtection_AvailableAndProtectedWhenReadSucceeds(t *testing.T) {
	obj := unstructuredNamespace("demo", map[string]string{"protect-deletion": "true"})
	s := serviceWithK8sClient("preprod", kubernetes.NewTestStatusClient(obj))

	st := s.GetProtection(context.Background(), "demo", "preprod")
	if !st.Available {
		t.Fatal("expected Available=true, the read succeeded")
	}
	if !st.Protected {
		t.Fatal("expected Protected=true, the annotation is set")
	}
}

// A read that fails must not read the same as "not protected": Available has
// to say so, or a caller checking only Protected would take a false negative
// for a fact.
func TestGetProtection_UnavailableWhenReadFails(t *testing.T) {
	k8s := kubernetes.NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "get" && action.GetResource().Resource == "namespaces" {
			return true, nil, errors.New("api server unreachable")
		}
		return false, nil, nil
	}, unstructuredNamespace("demo", map[string]string{"protect-deletion": "true"}))
	s := serviceWithK8sClient("preprod", k8s)

	st := s.GetProtection(context.Background(), "demo", "preprod")
	if st.Available {
		t.Fatal("expected Available=false, the read failed")
	}
	if st.Protected {
		t.Fatal("expected Protected=false when the read is unavailable, not a stale true")
	}
}
