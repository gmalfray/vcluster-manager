package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

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
