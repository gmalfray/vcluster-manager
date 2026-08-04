package service

import (
	"context"
	"errors"
	"testing"
)

func TestGetStatus_K8sUnavailable(t *testing.T) {
	s := newTestService()
	_, err := s.GetStatus(context.Background(), "demo", "preprod")
	if !errors.Is(err, ErrK8sUnavailable) {
		t.Fatalf("expected ErrK8sUnavailable when no client is configured, got %v", err)
	}
}

func TestGetStatus_K8sUnavailable_EnvDefaultsToPreprod(t *testing.T) {
	s := newTestService()
	_, err := s.GetStatus(context.Background(), "demo", "")
	if !errors.Is(err, ErrK8sUnavailable) {
		t.Fatalf("expected ErrK8sUnavailable, got %v", err)
	}
}

func TestGetFluxSummary_NoClients(t *testing.T) {
	s := newTestService()
	summary := s.GetFluxSummary(context.Background())
	if summary != (FluxSummary{}) {
		t.Fatalf("expected a zero summary when no client is configured, got %+v", summary)
	}
}

func TestGetQuotaForm_NotFound(t *testing.T) {
	fg := newFakeGitLab()
	// No values.yaml for "demo" → the parser fails to read it.
	s := newAppsTestService(t, fg)

	_, err := s.GetQuotaForm(context.Background(), "preprod", "demo")
	if !errors.Is(err, ErrVClusterNotFound) {
		t.Fatalf("expected ErrVClusterNotFound, got %v", err)
	}
}

func TestGetQuotaForm_Found(t *testing.T) {
	fg := newFakeGitLab()
	fg.addTree("preprod", "clusters/preprod/vclusters", "clusters/preprod/vclusters/demo/values.yaml")
	fg.addFile("preprod", "clusters/preprod/vclusters/demo/values.yaml", "veleroBackup:\n  enabled: true\n")
	s := newAppsTestService(t, fg)

	vc, err := s.GetQuotaForm(context.Background(), "preprod", "demo")
	if err != nil {
		t.Fatalf("GetQuotaForm: %v", err)
	}
	if vc.Name != "demo" || vc.Env != "preprod" {
		t.Fatalf("expected Name=demo Env=preprod, got Name=%q Env=%q", vc.Name, vc.Env)
	}
}

func TestGetQuotaForm_EnvDefaultsToPreprod(t *testing.T) {
	fg := newFakeGitLab()
	fg.addTree("preprod", "clusters/preprod/vclusters", "clusters/preprod/vclusters/demo/values.yaml")
	fg.addFile("preprod", "clusters/preprod/vclusters/demo/values.yaml", "")
	s := newAppsTestService(t, fg)

	vc, err := s.GetQuotaForm(context.Background(), "", "demo")
	if err != nil {
		t.Fatalf("GetQuotaForm: %v", err)
	}
	if vc.Env != "preprod" {
		t.Fatalf("expected empty env to default to preprod, got %q", vc.Env)
	}
}
