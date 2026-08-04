package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// Rancher-domain sentinel errors. Each adapter maps them to its own transport
// (web = toast + status ; REST = HTTP status + JSON). They exist so the exact
// guard messages the handler produced before the extraction can still be
// reproduced without leaking HTTP/HTML concerns into the service.
var (
	// ErrRancherNotConfigured means no Rancher client is configured at all.
	ErrRancherNotConfigured = errors.New("rancher client not configured")
	// ErrRancherNotEnabled means Rancher is not enabled for the environment.
	ErrRancherNotEnabled = errors.New("rancher not enabled for environment")
	// ErrCleaningInProgress means a Rancher cleanup job is already running.
	ErrCleaningInProgress = errors.New("rancher cleanup in progress")
	// ErrRancherK8sProdUnavailable means the prod Kubernetes client (needed to
	// apply the registration manifest) is not available.
	ErrRancherK8sProdUnavailable = errors.New("kubernetes prod client unavailable")
	// ErrAlreadyPaired means the vcluster is already registered and active in Rancher.
	ErrAlreadyPaired = errors.New("vcluster already paired in rancher")
	// ErrManualPairing means Rancher agents are already active in the vcluster
	// (a manual pairing was detected).
	ErrManualPairing = errors.New("rancher agents already active (manual pairing)")
)

// AlreadyExistsError is returned when the vcluster already exists in Rancher in
// a non-active state. It carries the observed State so the adapter can render
// the exact same message as before.
type AlreadyExistsError struct {
	State string
}

func (e *AlreadyExistsError) Error() string {
	return fmt.Sprintf("vcluster already exists in rancher (state: %s)", e.State)
}

// RancherOpError wraps an underlying Rancher API error for a named operation
// ("lookup" or "delete"), so the adapter can reproduce the exact prefixed toast.
type RancherOpError struct {
	Op  string
	Err error
}

func (e *RancherOpError) Error() string { return e.Op + " rancher: " + e.Err.Error() }
func (e *RancherOpError) Unwrap() error { return e.Err }

// RancherStatus is the Rancher pairing status of a vcluster. It is the single
// result type returned to both adapters (rendered as the rancher_status.html
// HTMX fragment by the web layer, serialized as JSON by the REST layer).
type RancherStatus struct {
	// Enabled reports whether Rancher is configured and enabled for the env.
	Enabled bool `json:"enabled"`
	// Paired means the cluster exists in Rancher and is active.
	Paired bool `json:"paired"`
	// Pairing means the cluster exists but is not yet active (import in progress).
	Pairing bool `json:"pairing"`
	// Unknown means the Rancher status could not be determined (lookup failed).
	Unknown bool `json:"unknown"`
	// ManuallyPaired means Rancher agents were detected via K8s pod labels while
	// no matching cluster was found by name (manual pairing under a different name).
	ManuallyPaired bool `json:"manually_paired"`
	// Cleaning means a rancher-cleanup job is currently running for the vcluster.
	Cleaning bool   `json:"cleaning"`
	Name     string `json:"name"`
	Env      string `json:"env"`
}

// GetRancherStatus reports the current Rancher pairing state of a vcluster.
// Read-only, no privilege required. It never returns an error: any lookup
// failure is projected onto the Unknown state so the caller always has a
// fragment to show.
func (s *Service) GetRancherStatus(ctx context.Context, name, env string) RancherStatus {
	env = envOrDefault(env)

	if s.rancher == nil || !s.cfg.RancherEnabledForEnv(env) {
		return RancherStatus{Enabled: false}
	}

	cleaning := s.cfg.IsCleaning(name, env)

	info, found, err := s.rancher.FindClusterByName(name)
	if err != nil {
		slog.Warn("rancher lookup failed", "vcluster", name, "err", err)
		// Explicit "unknown" state so the user knows the status could not be
		// determined, instead of silently showing "Off" which could lead to
		// accidental re-pairing.
		return RancherStatus{Enabled: true, Unknown: true, Cleaning: cleaning, Name: name, Env: env}
	}

	// Secondary check: look for Rancher agent pods synced to the host cluster's
	// vcluster namespace. vcluster mirrors pods from inside the virtual cluster
	// with a label indicating their original namespace (cattle-system). This
	// catches manual pairings where the Rancher cluster name doesn't follow the
	// vcluster-{name} convention and would not be found by FindClusterByName.
	if !found {
		if k8s := s.k8sForEnv(env); k8s != nil {
			if k8s.HasRancherAgents(ctx, name) {
				slog.Info("rancher: detected agents via K8s pod labels (manual pairing with different cluster name)", "env", env, "vcluster", name)
				return RancherStatus{Enabled: true, ManuallyPaired: true, Cleaning: cleaning, Name: name, Env: env}
			}
		}
	}

	// Cluster exists but not yet active = still pairing.
	paired := found && info.State == "active"
	pairing := found && !paired

	return RancherStatus{Enabled: true, Paired: paired, Pairing: pairing, Cleaning: cleaning, Name: name, Env: env}
}

// PairRancher registers a vcluster in Rancher (prod only, admin only). RBAC is
// enforced here so both adapters inherit it. On success it returns the
// immediate "pairing in progress" state and launches the heavy
// import/apply/wait work in a background goroutine. The returned error is one
// of the domain sentinels above (mapped to the same toast/HTTP status by the
// adapters).
func (s *Service) PairRancher(ctx context.Context, actor models.Actor, name, env string) (RancherStatus, error) {
	if !actor.IsAdmin {
		return RancherStatus{}, ErrForbidden
	}
	env = envOrDefault(env)

	if s.rancher == nil {
		return RancherStatus{}, ErrRancherNotConfigured
	}
	if !s.cfg.RancherEnabledForEnv(env) {
		return RancherStatus{}, ErrRancherNotEnabled
	}
	if s.cfg.IsCleaning(name, env) {
		return RancherStatus{}, ErrCleaningInProgress
	}

	k8s := s.k8sForEnv("prod")
	if k8s == nil {
		return RancherStatus{}, ErrRancherK8sProdUnavailable
	}

	// Defensive check: verify the vcluster is not already registered in
	// Rancher. This protects against accidental double-pairing when a
	// vcluster was paired manually.
	if existingInfo, found, err := s.rancher.FindClusterByName(name); err == nil && found {
		if existingInfo.State == "active" {
			return RancherStatus{}, ErrAlreadyPaired
		}
		return RancherStatus{}, &AlreadyExistsError{State: existingInfo.State}
	}

	// Secondary check: detect manual pairings via Rancher agent pods in the
	// host cluster.
	if k8sEnv := s.k8sForEnv(env); k8sEnv != nil {
		if k8sEnv.HasRancherAgents(ctx, name) {
			return RancherStatus{}, ErrManualPairing
		}
	}

	audit.LogActor(actor.Username, "pair-rancher", name, env)

	// Run the pairing asynchronously (heavy operation).
	go func() {
		// 1. Import cluster in Rancher.
		clusterID, manifestURL, err := s.rancher.ImportCluster(name)
		if err != nil {
			slog.Error("rancher: import failed", "vcluster", name, "err", err)
			return
		}
		slog.Info("rancher: cluster imported", "vcluster", name, "cluster_id", clusterID, "manifest", manifestURL)

		// 2. Download the registration manifest.
		manifest, err := s.rancher.DownloadManifest(manifestURL)
		if err != nil {
			slog.Error("rancher: download manifest failed", "vcluster", name, "err", err)
			return
		}

		// 3. Apply manifest inside the vcluster via port-forward (works for
		// same-cluster and cross-cluster).
		bg := context.Background()
		if err := k8s.ApplyManifestToVClusterViaPortForward(bg, name, manifest); err != nil {
			slog.Error("rancher: apply manifest failed", "vcluster", name, "err", err)
			return
		}
		slog.Info("rancher: manifest applied, waiting for cluster to become active", "vcluster", name)

		// 4. Wait for the cluster to become active in Rancher (agent connects back).
		if err := s.rancher.WaitForClusterActive(clusterID, 5*time.Minute); err != nil {
			slog.Error("rancher: cluster did not become active", "vcluster", name, "err", err)
			return
		}

		slog.Info("rancher: vcluster successfully paired and active", "vcluster", name)
	}()

	return RancherStatus{Enabled: true, Pairing: true, Name: name, Env: env}, nil
}

// UnpairRancher removes a vcluster from Rancher (prod only, admin only). RBAC
// is enforced here. It deletes the Rancher cluster (when found) then launches
// the rancher-cleanup job inside the vcluster asynchronously, returning the
// immediate "cleaning in progress" state.
func (s *Service) UnpairRancher(ctx context.Context, actor models.Actor, name, env string) (RancherStatus, error) {
	if !actor.IsAdmin {
		return RancherStatus{}, ErrForbidden
	}
	env = envOrDefault(env)

	if s.rancher == nil {
		return RancherStatus{}, ErrRancherNotConfigured
	}
	if !s.cfg.RancherEnabledForEnv(env) {
		return RancherStatus{}, ErrRancherNotEnabled
	}

	// 1. Find cluster in Rancher (may not be found if paired manually with a
	// different name, or if already deleted from Rancher side — we still
	// proceed with vcluster cleanup).
	info, found, err := s.rancher.FindClusterByName(name)
	if err != nil {
		return RancherStatus{}, &RancherOpError{Op: "lookup", Err: err}
	}

	audit.LogActor(actor.Username, "unpair-rancher", name, env)

	// 2. Delete cluster from Rancher (only if found — if the cluster was
	// deleted from Rancher manually or paired with a different name, skip
	// Rancher deletion and go straight to cleanup).
	if found {
		if err := s.rancher.DeleteCluster(info.ID); err != nil {
			return RancherStatus{}, &RancherOpError{Op: "delete", Err: err}
		}
		slog.Info("rancher: cluster deleted", "vcluster", name, "cluster_id", info.ID)
	} else {
		slog.Info("rancher: cluster not found, skipping Rancher deletion (may have been deleted manually or paired with a different name)", "vcluster", name)
	}

	// 3. Deploy rancher-cleanup job in the vcluster via port-forward.
	k8s := s.k8sForEnv(env)
	if k8s != nil {
		s.cfg.AddCleaning(name, env, false, false, false, false)
		go func() {
			bg := context.Background()
			if err := k8s.ApplyManifestToVClusterViaPortForward(bg, name, []byte(rancherCleanupManifest)); err != nil {
				slog.Warn("could not deploy rancher-cleanup in vcluster", "vcluster", name, "err", err)
				s.cfg.RemoveCleaning(name, env)
				return
			}
			slog.Info("rancher: cleanup job deployed, waiting for completion", "vcluster", name)

			if err := k8s.WaitForJobComplete(bg, name, "rancher-cleanup", "kube-system", 10*time.Minute); err != nil {
				slog.Warn("rancher-cleanup job did not complete", "vcluster", name, "err", err)
			} else {
				slog.Info("rancher: cleanup completed", "vcluster", name)
			}
			s.cfg.RemoveCleaning(name, env)
		}()
	}

	return RancherStatus{Enabled: true, Cleaning: true, Name: name, Env: env}, nil
}

// rancherCleanupManifest is the official rancher-cleanup job manifest.
// See https://github.com/rancher/rancher-cleanup
const rancherCleanupManifest = `---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cleanup-service-account
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cleanup-admin
subjects:
- kind: ServiceAccount
  name: cleanup-service-account
  namespace: kube-system
roleRef:
  kind: ClusterRole
  name: cluster-admin
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: batch/v1
kind: Job
metadata:
  name: rancher-cleanup
  namespace: kube-system
  labels:
    app: rancher-cleanup
spec:
  ttlSecondsAfterFinished: 300
  template:
    spec:
      containers:
      - name: cleanup
        image: rancher/rancher-cleanup:latest
        args: ["force"]
        imagePullPolicy: Always
      serviceAccountName: cleanup-service-account
      restartPolicy: Never
  backoffLimit: 4
`
