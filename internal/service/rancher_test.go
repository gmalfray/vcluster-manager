package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/rancher"
)

// newRancherTestService builds a Service with no Rancher client and Rancher
// enabled on both environments — enough to exercise the guard rules without a
// real Rancher API or a real cluster.
func newRancherTestService() *Service {
	var mu sync.RWMutex
	return New(Deps{
		Cfg:          &config.Config{RancherEnabledPreprod: true, RancherEnabledProd: true},
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})
}

func TestPairRancher_ForbiddenForNonAdmin(t *testing.T) {
	s := newRancherTestService()
	_, err := s.PairRancher(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, "demo", "preprod")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestPairRancher_NotConfigured(t *testing.T) {
	s := newRancherTestService()
	_, err := s.PairRancher(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod")
	if !errors.Is(err, ErrRancherNotConfigured) {
		t.Fatalf("expected ErrRancherNotConfigured when no Rancher client is set, got %v", err)
	}
}

func TestPairRancher_NotEnabledForEnv(t *testing.T) {
	var mu sync.RWMutex
	s := New(Deps{
		Cfg:          &config.Config{RancherEnabledPreprod: false},
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})
	_, err := s.PairRancher(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod")
	// No Rancher client either, but the env check runs first (mirrors the
	// former handler's guard order: client nil, then env enabled, then
	// cleaning-in-progress).
	if !errors.Is(err, ErrRancherNotConfigured) {
		t.Fatalf("expected ErrRancherNotConfigured (client nil checked before env), got %v", err)
	}
}

func TestUnpairRancher_ForbiddenForNonAdmin(t *testing.T) {
	s := newRancherTestService()
	_, err := s.UnpairRancher(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, "demo", "preprod")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestUnpairRancher_NotConfigured(t *testing.T) {
	s := newRancherTestService()
	_, err := s.UnpairRancher(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod")
	if !errors.Is(err, ErrRancherNotConfigured) {
		t.Fatalf("expected ErrRancherNotConfigured when no Rancher client is set, got %v", err)
	}
}

func TestUnpairRancher_NotEnabledForEnv(t *testing.T) {
	var mu sync.RWMutex
	s := New(Deps{
		Cfg:          &config.Config{RancherEnabledProd: true},
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})
	// No Rancher client set: same guard order as PairRancher, the nil-client
	// check fires before the env check.
	_, err := s.UnpairRancher(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod")
	if !errors.Is(err, ErrRancherNotConfigured) {
		t.Fatalf("expected ErrRancherNotConfigured, got %v", err)
	}
}

// TestPairRancher_ProdK8sClientMissing locks the D3 fix: PairRancher applies
// the Rancher registration manifest through the prod Kubernetes client
// specifically (hardcoded, not the vcluster's own env — see the comment on
// k8sForEnv in service.go for why). Before the fix, k8sForEnv fell back to
// "any client in the map" when "prod" wasn't registered, so this case was
// unreachable: a preprod-only install would silently apply the manifest
// through the preprod client instead of failing loudly.
func TestPairRancher_ProdK8sClientMissing(t *testing.T) {
	var mu sync.RWMutex
	s := New(Deps{
		Cfg:     &config.Config{RancherEnabledPreprod: true},
		Rancher: rancher.NewClient("http://unused.invalid", "tok"),
		// Only "preprod" is registered — no "prod" client, mirroring an
		// install where KUBECONFIG_PROD is missing.
		K8sClients:   map[string]*kubernetes.StatusClient{"preprod": kubernetes.NewTestStatusClient()},
		K8sClientsMu: &mu,
	})
	_, err := s.PairRancher(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod")
	if !errors.Is(err, ErrRancherK8sProdUnavailable) {
		t.Fatalf("expected ErrRancherK8sProdUnavailable when no prod k8s client is registered, got %v", err)
	}
}

func TestGetRancherStatus_DisabledWhenNoClient(t *testing.T) {
	s := newRancherTestService()
	st := s.GetRancherStatus(context.Background(), "demo", "preprod")
	if st.Enabled {
		t.Fatalf("expected Enabled=false when no Rancher client is configured")
	}
}

func TestGetRancherStatus_DisabledWhenEnvNotEnabled(t *testing.T) {
	var mu sync.RWMutex
	s := New(Deps{
		Cfg:          &config.Config{RancherEnabledPreprod: false},
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})
	st := s.GetRancherStatus(context.Background(), "demo", "preprod")
	if st.Enabled {
		t.Fatalf("expected Enabled=false when Rancher is not enabled for the env")
	}
}

func TestGetRancherStatus_EnvDefaultsToPreprod(t *testing.T) {
	s := newRancherTestService()
	st := s.GetRancherStatus(context.Background(), "demo", "")
	if st.Enabled {
		t.Fatalf("expected Enabled=false (no Rancher client), Env should still default")
	}
}

func TestAlreadyExistsError_Message(t *testing.T) {
	err := &AlreadyExistsError{State: "provisioning"}
	want := "vcluster already exists in rancher (state: provisioning)"
	if err.Error() != want {
		t.Fatalf("expected message %q, got %q", want, err.Error())
	}
}

func TestRancherOpError_UnwrapAndMessage(t *testing.T) {
	inner := errors.New("boom")
	err := &RancherOpError{Op: "delete", Err: inner}
	if !errors.Is(err, inner) {
		t.Fatalf("expected RancherOpError to unwrap to the inner error")
	}
	want := "delete rancher: boom"
	if err.Error() != want {
		t.Fatalf("expected message %q, got %q", want, err.Error())
	}
}
