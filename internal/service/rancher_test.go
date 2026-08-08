package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeRancherFindHandler answers FindClusterByName's GET /v3/clusters?name=…
// with an empty result: the vcluster isn't registered in Rancher. block, if
// non-nil, is closed to let a POST /v3/clusters (ImportCluster) go through —
// used to hold PairRancher's background goroutine at the starting line while
// the test asserts on what already happened synchronously.
func fakeRancherFindHandler(t *testing.T, block <-chan struct{}) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		case http.MethodPost:
			if block != nil {
				<-block
			}
			// Any non-2xx makes ImportCluster fail immediately, with no
			// retry sleep — the request never gets past the status check.
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

// TestPairRancher_ClearsPreviousFailureBeforeLaunchingNewAttempt locks the
// "a new attempt starts clean" half of the fix: PairRancher clears whatever
// a previous cycle left behind as soon as it commits to a fresh attempt —
// synchronously, before the background goroutine that does the real work
// even starts, not whenever that goroutine happens to get around to it.
func TestPairRancher_ClearsPreviousFailureBeforeLaunchingNewAttempt(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(fakeRancherFindHandler(t, block))
	defer srv.Close()

	cfg := newTestConfig(t)
	cfg.RancherEnabledPreprod = true
	cfg.SetRancherPairingFailure("demo", "preprod", "échec d'une tentative précédente")

	var mu sync.RWMutex
	s := New(Deps{
		Cfg:          cfg,
		Rancher:      rancher.NewClient(srv.URL, "tok"),
		K8sClients:   map[string]*kubernetes.StatusClient{"prod": kubernetes.NewTestStatusClient()},
		K8sClientsMu: &mu,
	})

	if _, err := s.PairRancher(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod"); err != nil {
		t.Fatalf("PairRancher: %v", err)
	}

	// The background goroutine is still stuck on its POST — if the record is
	// already gone, it can only be the synchronous clear that did it.
	if _, recorded := cfg.RancherPairingFailureFor("demo", "preprod"); recorded {
		t.Fatal("the previous pairing failure is still recorded after a new attempt started")
	}

	close(block) // let the blocked import go through (and fail: server always 500s)

	// Wait for the goroutine to actually finish writing before the test
	// returns and its temp dir gets torn down — otherwise the cleanup races
	// the goroutine's own file write.
	waitForRancherPairingFailure(t, cfg, "demo", "preprod")
}

// TestPairRancher_GoroutineFailureIsPersisted locks the other half: when the
// background goroutine fails, the failure must reach cfg (not just slog),
// or the next GetRancherStatus poll has nothing to show for it.
func TestPairRancher_GoroutineFailureIsPersisted(t *testing.T) {
	srv := httptest.NewServer(fakeRancherFindHandler(t, nil))
	defer srv.Close()

	cfg := newTestConfig(t)
	cfg.RancherEnabledPreprod = true

	var mu sync.RWMutex
	s := New(Deps{
		Cfg:          cfg,
		Rancher:      rancher.NewClient(srv.URL, "tok"),
		K8sClients:   map[string]*kubernetes.StatusClient{"prod": kubernetes.NewTestStatusClient()},
		K8sClientsMu: &mu,
	})

	if _, err := s.PairRancher(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod"); err != nil {
		t.Fatalf("PairRancher: %v", err)
	}

	failure := waitForRancherPairingFailure(t, cfg, "demo", "preprod")
	if !strings.Contains(failure.Message, "import Rancher") {
		t.Fatalf("expected the message to say the import step failed, got %q", failure.Message)
	}
	if failure.At == "" {
		t.Fatal("recorded failure has no timestamp")
	}
}

// TestPairRancher_ApplyManifestFailureIsPersisted reproduces the 2026-08-08
// incident end to end, minus the RBAC part: import and manifest download
// succeed — the cluster gets created in Rancher, so from then on it sits
// there "pending" forever — but applying the manifest inside the vcluster
// fails, here because no internal kubeconfig secret exists (standing in for
// the missing RBAC that caused the real outage; both make
// ApplyManifestToVClusterViaPortForward fail the same way). The failure at
// this specific step must reach cfg, same as the other three.
func TestPairRancher_ApplyManifestFailureIsPersisted(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/clusters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"c-1","name":"vcluster-demo","state":"pending"}`))
		}
	})
	mux.HandleFunc("/v3/clusterregistrationtokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"t-1","manifestUrl":"` + srv.URL + `/manifest.yaml"}`))
	})
	mux.HandleFunc("/manifest.yaml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: cattle-system\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	cfg := newTestConfig(t)
	cfg.RancherEnabledPreprod = true

	var mu sync.RWMutex
	s := New(Deps{
		Cfg:     cfg,
		Rancher: rancher.NewClient(srv.URL, "tok"),
		// No secret vc-vcluster-demo-int seeded: getInternalKubeconfig fails,
		// same shape of failure as the RBAC gap that caused the real outage.
		K8sClients:   map[string]*kubernetes.StatusClient{"prod": kubernetes.NewTestStatusClient()},
		K8sClientsMu: &mu,
	})

	if _, err := s.PairRancher(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod"); err != nil {
		t.Fatalf("PairRancher: %v", err)
	}

	failure := waitForRancherPairingFailure(t, cfg, "demo", "preprod")
	if !strings.Contains(failure.Message, "application du manifeste") {
		t.Fatalf("expected the message to say the apply step failed, got %q", failure.Message)
	}
}

// TestFinishPairingSuccess_ClearsPairingFailure: a pairing attempt that
// eventually goes all the way to active must not leave an earlier attempt's
// failure lying around — otherwise, if the cluster later drops back out of
// "active" on its own (agent disconnects, say), GetRancherStatus would blame
// the stall on a cause that has nothing to do with it.
func TestFinishPairingSuccess_ClearsPairingFailure(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.SetRancherPairingFailure("demo", "preprod", "échec d'une tentative précédente")

	s := &Service{cfg: cfg}
	s.finishPairingSuccess("demo", "preprod")

	if _, recorded := cfg.RancherPairingFailureFor("demo", "preprod"); recorded {
		t.Fatal("expected the pairing failure to be cleared once an attempt succeeds")
	}
}

// waitForRancherPairingFailure polls cfg for name/env's recorded pairing
// failure until it shows up, up to 8s (ImportCluster alone sleeps 2s before
// its first attempt at a registration token). PairRancher's background
// goroutine runs on its own schedule, so tests that trigger one need to wait
// for it rather than assert immediately after the request returns — and
// waiting here (instead of just returning once the assertion passes) doubles
// as letting the goroutine finish before the test's t.TempDir gets torn down.
func waitForRancherPairingFailure(t *testing.T, cfg *config.Config, name, env string) config.RancherPairingFailure {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if failure, recorded := cfg.RancherPairingFailureFor(name, env); recorded {
			return failure
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s/%s's pairing failure to be recorded", name, env)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGetRancherStatus_SurfacesLastPairingFailureWhenNotPaired makes sure the
// persisted failure actually reaches the UI-facing status, not just cfg: a
// stuck "Pairing" state and a silently failed one look identical without it.
func TestGetRancherStatus_SurfacesLastPairingFailureWhenNotPaired(t *testing.T) {
	srv := httptest.NewServer(fakeRancherFindHandler(t, nil))
	defer srv.Close()

	cfg := newTestConfig(t)
	cfg.RancherEnabledPreprod = true
	cfg.SetRancherPairingFailure("demo", "preprod", "le cluster n'est pas devenu actif : timeout")

	var mu sync.RWMutex
	s := New(Deps{
		Cfg:          cfg,
		Rancher:      rancher.NewClient(srv.URL, "tok"),
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})

	st := s.GetRancherStatus(context.Background(), "demo", "preprod")
	if st.LastPairingError == "" {
		t.Fatal("expected LastPairingError to be populated from the persisted failure")
	}
	if st.LastPairingErrorAt == "" {
		t.Fatal("expected LastPairingErrorAt to be populated")
	}
}

// TestGetRancherStatus_HidesLastPairingFailureWhenPaired: a stale failure
// from a since-resolved attempt must not follow a vcluster that is now
// actually paired and active — it would read as a live problem that isn't one.
func TestGetRancherStatus_HidesLastPairingFailureWhenPaired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/clusters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"c-1","name":"vcluster-demo","state":"active"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := newTestConfig(t)
	cfg.RancherEnabledPreprod = true
	cfg.SetRancherPairingFailure("demo", "preprod", "stale from a previous cycle")

	var mu sync.RWMutex
	s := New(Deps{
		Cfg:          cfg,
		Rancher:      rancher.NewClient(srv.URL, "tok"),
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})

	st := s.GetRancherStatus(context.Background(), "demo", "preprod")
	if !st.Paired {
		t.Fatalf("expected Paired=true, got %+v", st)
	}
	if st.LastPairingError != "" {
		t.Fatalf("expected no LastPairingError once the vcluster is paired, got %q", st.LastPairingError)
	}
}

// TestUnpairRancher_ClearsPairingFailure: unpairing is the documented escape
// hatch out of a stuck pairing. Once taken, the old failure no longer
// describes anything relevant — it should not haunt the next attempt.
func TestUnpairRancher_ClearsPairingFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/clusters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := newTestConfig(t)
	cfg.RancherEnabledPreprod = true
	cfg.SetRancherPairingFailure("demo", "preprod", "échec précédent")

	var mu sync.RWMutex
	s := New(Deps{
		Cfg:          cfg,
		Rancher:      rancher.NewClient(srv.URL, "tok"),
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})

	if _, err := s.UnpairRancher(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod"); err != nil {
		t.Fatalf("UnpairRancher: %v", err)
	}

	if _, recorded := cfg.RancherPairingFailureFor("demo", "preprod"); recorded {
		t.Fatal("expected the pairing failure to be cleared after an explicit unpair")
	}
}
