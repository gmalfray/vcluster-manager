package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/audit"
)

// PairRancher registers a vcluster in Rancher (prod only, admin only).
func (h *Handlers) PairRancher(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	if h.rancher == nil {
		h.renderToast(w, "error", "Client Rancher non configure")
		return
	}

	if !h.cfg.RancherEnabledForEnv(env) {
		h.renderToast(w, "error", fmt.Sprintf("Rancher n'est pas active pour l'environnement %s", env))
		return
	}

	if h.cfg.IsCleaning(name, env) {
		h.renderToast(w, "error", "Nettoyage Rancher en cours, veuillez patienter")
		return
	}

	k8s := h.k8sForEnv("prod")
	if k8s == nil {
		h.renderToast(w, "error", "Client Kubernetes prod non disponible")
		return
	}

	// Defensive check: verify the vcluster is not already registered in Rancher.
	// This protects against accidental double-pairing when a vcluster was paired manually.
	if existingInfo, found, err := h.rancher.FindClusterByName(name); err == nil && found {
		if existingInfo.State == "active" {
			h.renderToast(w, "error", "Ce vcluster est déjà appairé dans Rancher (état: active). Désappairez-le d'abord.")
		} else {
			h.renderToast(w, "error", fmt.Sprintf("Ce vcluster existe déjà dans Rancher (état: %s). Attendez ou désappairez-le d'abord.", existingInfo.State))
		}
		return
	}

	// Secondary check: detect manual pairings via Rancher agent pods in the host cluster.
	if k8sProd := h.k8sForEnv(env); k8sProd != nil {
		if k8sProd.HasRancherAgents(r.Context(), name) {
			h.renderToast(w, "error", "Des agents Rancher sont déjà actifs dans ce vcluster (appairage manuel détecté). Désappairez-le d'abord.")
			return
		}
	}

	audit.Log(r, "pair-rancher", name, env)
	// Show pairing in progress immediately
	h.renderPartial(w, "rancher_status.html", map[string]interface{}{
		"Enabled": true,
		"Paired":  false,
		"Pairing": true,
		"Name":    name,
		"Env":     env,
	})

	// Run the pairing asynchronously (heavy operation)
	go func() {
		// 1. Import cluster in Rancher
		clusterID, manifestURL, err := h.rancher.ImportCluster(name)
		if err != nil {
			slog.Error("rancher: import failed", "vcluster", name, "err", err)
			return
		}
		slog.Info("rancher: cluster imported", "vcluster", name, "cluster_id", clusterID, "manifest", manifestURL)

		// 2. Download the registration manifest
		manifest, err := h.rancher.DownloadManifest(manifestURL)
		if err != nil {
			slog.Error("rancher: download manifest failed", "vcluster", name, "err", err)
			return
		}

		// 3. Apply manifest inside the vcluster via port-forward
		// Always use port-forward for Rancher (works for same-cluster and cross-cluster)
		ctx := context.Background()
		if err := k8s.ApplyManifestToVClusterViaPortForward(ctx, name, manifest); err != nil {
			slog.Error("rancher: apply manifest failed", "vcluster", name, "err", err)
			return
		}
		slog.Info("rancher: manifest applied, waiting for cluster to become active", "vcluster", name)

		// 4. Wait for the cluster to become active in Rancher (agent connects back)
		if err := h.rancher.WaitForClusterActive(clusterID, 5*time.Minute); err != nil {
			slog.Error("rancher: cluster did not become active", "vcluster", name, "err", err)
			return
		}

		slog.Info("rancher: vcluster successfully paired and active", "vcluster", name)
	}()
}

// UnpairRancher removes a vcluster from Rancher (prod only, admin only).
func (h *Handlers) UnpairRancher(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	if h.rancher == nil {
		h.renderToast(w, "error", "Client Rancher non configure")
		return
	}

	if !h.cfg.RancherEnabledForEnv(env) {
		h.renderToast(w, "error", fmt.Sprintf("Rancher n'est pas active pour l'environnement %s", env))
		return
	}

	// 1. Find cluster in Rancher (may not be found if paired manually with a different name,
	// or if already deleted from Rancher side — we still proceed with vcluster cleanup).
	info, found, err := h.rancher.FindClusterByName(name)
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur recherche Rancher : %v", err))
		return
	}

	audit.Log(r, "unpair-rancher", name, env)
	// 2. Delete cluster from Rancher (only if found — if the cluster was deleted from Rancher
	// manually or paired with a different name, skip Rancher deletion and go straight to cleanup).
	if found {
		if err := h.rancher.DeleteCluster(info.ID); err != nil {
			h.renderToast(w, "error", fmt.Sprintf("Erreur suppression Rancher : %v", err))
			return
		}
		slog.Info("rancher: cluster deleted", "vcluster", name, "cluster_id", info.ID)
	} else {
		slog.Info("rancher: cluster not found, skipping Rancher deletion (may have been deleted manually or paired with a different name)", "vcluster", name)
	}

	// 3. Deploy rancher-cleanup job in the vcluster via port-forward
	k8s := h.k8sForEnv(env)
	if k8s != nil {
		h.cfg.AddCleaning(name, env, false, false, false, false)
		go func() {
			ctx := context.Background()
			if err := k8s.ApplyManifestToVClusterViaPortForward(ctx, name, []byte(rancherCleanupManifest)); err != nil {
				slog.Warn("could not deploy rancher-cleanup in vcluster", "vcluster", name, "err", err)
				h.cfg.RemoveCleaning(name, env)
				return
			}
			slog.Info("rancher: cleanup job deployed, waiting for completion", "vcluster", name)

			if err := k8s.WaitForJobComplete(ctx, name, "rancher-cleanup", "kube-system", 10*time.Minute); err != nil {
				slog.Warn("rancher-cleanup job did not complete", "vcluster", name, "err", err)
			} else {
				slog.Info("rancher: cleanup completed", "vcluster", name)
			}
			h.cfg.RemoveCleaning(name, env)
		}()
	}

	// Return updated status
	h.renderPartial(w, "rancher_status.html", map[string]interface{}{
		"Enabled":  true,
		"Paired":   false,
		"Cleaning": true,
		"Name":     name,
		"Env":      env,
	})
}

// RancherStatus returns an HTMX fragment with the current Rancher pairing status.
func (h *Handlers) RancherStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	if h.rancher == nil || !h.cfg.RancherEnabledForEnv(env) {
		h.renderPartial(w, "rancher_status.html", map[string]interface{}{
			"Enabled": false,
		})
		return
	}

	// Check if cleanup is in progress
	cleaning := h.cfg.IsCleaning(name, env)

	info, found, err := h.rancher.FindClusterByName(name)
	if err != nil {
		slog.Warn("Rancher lookup failed", "vcluster", name, "err", err)
		// Render an explicit "unknown" state so the user knows the status could not be determined,
		// instead of silently showing "Off" which could lead to accidental re-pairing.
		h.renderPartial(w, "rancher_status.html", map[string]interface{}{
			"Enabled":  true,
			"Unknown":  true,
			"Cleaning": cleaning,
			"Name":     name,
			"Env":      env,
		})
		return
	}

	// Secondary check: look for Rancher agent pods synced to the host cluster's vcluster namespace.
	// vcluster mirrors pods from inside the virtual cluster with a label indicating their original
	// namespace (cattle-system). This catches manual pairings where the Rancher cluster name
	// doesn't follow the vcluster-{name} convention and would not be found by FindClusterByName.
	if !found {
		if k8s := h.k8sForEnv(env); k8s != nil {
			if k8s.HasRancherAgents(r.Context(), name) {
				slog.Info("rancher: detected agents via K8s pod labels (manual pairing with different cluster name)", "env", env, "vcluster", name)
				h.renderPartial(w, "rancher_status.html", map[string]interface{}{
					"Enabled":        true,
					"ManuallyPaired": true,
					"Cleaning":       cleaning,
					"Name":           name,
					"Env":            env,
				})
				return
			}
		}
	}

	// Cluster exists but not yet active = still pairing
	paired := found && info.State == "active"
	pairing := found && !paired

	h.renderPartial(w, "rancher_status.html", map[string]interface{}{
		"Enabled":  true,
		"Paired":   paired,
		"Pairing":  pairing,
		"Cleaning": cleaning,
		"Name":     name,
		"Env":      env,
	})
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
