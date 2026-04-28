package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"gopkg.in/yaml.v3"
)

// RetryVaultSetup re-triggers the Vault auth backend setup for a given vcluster (admin only).
// Useful after a vcluster-manager restart or a previous setup failure.
func (h *Handlers) RetryVaultSetup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	if h.vault == nil {
		h.renderToast(w, "error", "Vault non configure")
		return
	}

	if k8s := h.k8sForEnv(env); k8s == nil {
		h.renderToast(w, "error", fmt.Sprintf("Client Kubernetes %s non disponible", env))
		return
	}

	// Prevent double-launch: if already running (waiting or configuring), skip
	if vs := h.getVaultState(env, name); vs != nil && (vs.Status == "waiting" || vs.Status == "configuring") {
		h.renderToast(w, "warning", "Setup Vault deja en cours pour ce vcluster")
		return
	}

	go h.setupVaultAuthWhenReady(name, env)
	h.renderToast(w, "success", fmt.Sprintf("Setup Vault relance pour %s (%s)", name, env))
}

// FluxSummaryFragment returns an HTMX fragment with HelmRelease counts for the dashboard Flux Status card.
func (h *Handlers) FluxSummaryFragment(w http.ResponseWriter, r *http.Request) {
	type envCount struct {
		Total int
		Ready int
	}
	counts := map[string]envCount{}

	for _, env := range []string{"preprod", "prod"} {
		k8s := h.k8sForEnv(env)
		if k8s == nil {
			continue
		}
		total, ready, err := k8s.CountReadyHelmReleases(r.Context())
		if err != nil {
			slog.Error("error counting helm releases", "env", env, "err", err)
			continue
		}
		counts[env] = envCount{Total: total, Ready: ready}
	}

	pp := counts["preprod"]
	pr := counts["prod"]
	h.renderPartial(w, "flux_summary.html", map[string]interface{}{
		"Total":        pp.Total + pr.Total,
		"Ready":        pp.Ready + pr.Ready,
		"PreprodTotal": pp.Total,
		"PreprodReady": pp.Ready,
		"ProdTotal":    pr.Total,
		"ProdReady":    pr.Ready,
	})
}

// StatusFragment returns an HTMX fragment with the vcluster status badge.
func (h *Handlers) StatusFragment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	// Handle deleting mode: check if HelmRelease still exists
	if r.URL.Query().Get("deleting") == "true" {
		k8s := h.k8sForEnv(env)
		if k8s != nil && !k8s.HelmReleaseExists(r.Context(), name) {
			// HelmRelease gone: remove any stuck FluxCD finalizers then cleanup
			if err := k8s.CleanupNamespace(r.Context(), name); err != nil {
				slog.Warn("namespace cleanup failed", "env", env, "vcluster", name, "err", err)
			}
			h.cfg.RemoveDeleting(name, env)
			go h.sendNotification(fmt.Sprintf("vcluster *%s* (%s) supprime avec succes.", name, env))
			w.Header().Set("HX-Redirect", "/")
			w.WriteHeader(http.StatusOK)
			return
		}
		// Still deleting
		h.renderPartial(w, "status_badge.html", map[string]interface{}{
			"Deleting": true,
		})
		return
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderPartial(w, "status_badge.html", map[string]interface{}{
			"HelmRelease":   "N/A",
			"Kustomization": "N/A",
		})
		return
	}

	status, err := k8s.GetVClusterStatus(r.Context(), name)
	if err != nil {
		h.renderPartial(w, "status_badge.html", map[string]interface{}{
			"HelmRelease":   "Error",
			"Kustomization": "Error",
		})
		return
	}

	configVersion := r.URL.Query().Get("configVersion")

	data := map[string]interface{}{
		"HelmRelease":    status.HelmRelease,
		"Kustomization":  status.FluxKustomization,
		"K8sVersion":     status.K8sVersion,
		"ConfigVersion":  configVersion,
		"CPUUsage":       status.CPUUsage,
		"MemoryUsage":    status.MemoryUsage,
		"StorageUsage":   status.StorageUsage,
		"CPUPercent":     status.CPUPercent,
		"MemoryPercent":  status.MemoryPercent,
		"StoragePercent": status.StoragePercent,
	}

	data["VaultEnabled"] = h.vault != nil
	data["VClusterName"] = name
	data["VClusterEnv"] = env
	if vs := h.getVaultState(env, name); vs != nil {
		data["VaultStatus"] = vs.Status
		data["VaultMessage"] = vs.Message
	}

	h.renderPartial(w, "status_badge.html", data)
}

// DownloadKubeconfig returns the vcluster external kubeconfig as a file download (admin only).
func (h *Handlers) DownloadKubeconfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		http.Error(w, "Kubernetes client not available for "+env, http.StatusServiceUnavailable)
		return
	}

	kubeconfig, err := k8s.GetKubeconfig(r.Context(), name)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get kubeconfig: %v", err), http.StatusInternalServerError)
		return
	}

	kubeconfig = renameKubeconfig(kubeconfig, name, env)

	filename := fmt.Sprintf("kubeconfig-vcluster-%s-%s.yaml", name, env)
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Write(kubeconfig)
}

// CreateAppManifestsRepo creates the app-manifests GitLab repo for a vcluster (admin only).
func (h *Handlers) CreateAppManifestsRepo(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	name := r.PathValue("name")

	if h.gitlab == nil {
		h.renderToast(w, "error", "GitLab client non disponible")
		return
	}

	if _, err := h.gitlab.CreateAppManifestsRepo(name); err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur creation repo : %v", err))
		return
	}

	h.redirectWithFlash(w, fmt.Sprintf("/vclusters/%s?env=%s", name, r.URL.Query().Get("env")), "success", fmt.Sprintf("Repo app-manifests-%s cree avec succes", name))
}

// CreateProdMR creates (or returns existing) the preprod→master MR for a pending vcluster.
func (h *Handlers) CreateProdMR(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if h.gitlab == nil {
		h.renderToast(w, "error", "GitLab client non disponible")
		return
	}
	mrDescription := fmt.Sprintf(
		"Promotion des changements de preprod vers la production (vcluster %s).\n\n"+
			"Créé automatiquement par vcluster-manager.\n\n---\n\n"+
			"> ℹ️ **Note sur le diff** : Ce MR contient des fichiers sous `clusters/preprod/` **et** `clusters/prod/`.\n"+
			"> Seuls les fichiers sous **`clusters/prod/`** ont un impact sur la production.\n"+
			"> Les fichiers `clusters/preprod/` sont présents car la branche **preprod est la source de vérité** pour les deux environnements.",
		name,
	)
	mrURL, err := h.gitlab.GetOrCreateMergeRequest(
		"preprod", "master",
		"feat: promote preprod to prod",
		mrDescription,
	)
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur création MR : %v", err))
		return
	}
	h.redirectWithFlash(w, fmt.Sprintf("/vclusters/%s?env=prod", name), "success", fmt.Sprintf("MR créée : %s", mrURL))
}

// QuotaForm returns the quota editing form fragment.
func (h *Handlers) QuotaForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	vc, err := h.parser.ParseVCluster(env, name)
	if err != nil {
		http.Error(w, "VCluster not found", http.StatusNotFound)
		return
	}

	h.renderPartial(w, "quota_form.html", map[string]interface{}{
		"VCluster": vc,
		"Env":      env,
	})
}

// UpdateChart handles POST /api/chart/update to update the vcluster chart.
func (h *Handlers) UpdateChart(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	version := r.FormValue("version")
	if version == "" {
		h.renderToast(w, "error", "Version manquante")
		return
	}

	if h.helmUpdater == nil {
		h.renderToast(w, "error", "Mise a jour du chart non configuree (GITLAB_HELM_PROJECT_ID manquant)")
		return
	}

	mrURL, err := h.helmUpdater.UpdateChart(version)
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur lors de la mise a jour : %v", err))
		return
	}

	audit.Log(r, "update-chart", "", "global", "version="+r.FormValue("version"))
	h.redirectWithFlash(w, "/", "success", fmt.Sprintf("Chart mis a jour sur preprod. MR prod : %s", mrURL))
}

// UpdateK8sVersion handles POST /api/chart/k8s-version to update the default K8s version.
func (h *Handlers) UpdateK8sVersion(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	version := r.FormValue("version")
	if version == "" {
		h.renderToast(w, "error", "Version K8s manquante")
		return
	}

	if h.helmUpdater == nil {
		h.renderToast(w, "error", "Mise a jour non configuree (GITLAB_HELM_PROJECT_ID manquant)")
		return
	}

	mrURL, err := h.helmUpdater.UpdateK8sVersion(version)
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur lors de la mise a jour K8s : %v", err))
		return
	}

	audit.Log(r, "update-k8s-version", "", "global", "version="+r.FormValue("version"))
	h.redirectWithFlash(w, "/", "success", fmt.Sprintf("Version K8s mise a jour sur preprod. MR prod : %s", mrURL))
}

// UpdateArgoCDVersion handles POST /api/argocd/update to update the global ArgoCD version.
func (h *Handlers) UpdateArgoCDVersion(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	version := r.FormValue("version")
	if version == "" {
		h.renderToast(w, "error", "Version ArgoCD manquante")
		return
	}

	if h.argocdUpdater == nil {
		h.renderToast(w, "error", "Mise a jour ArgoCD non configuree (GitLab client non disponible)")
		return
	}

	mrURL, err := h.argocdUpdater.UpdateGlobalVersion(version)
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur lors de la mise a jour ArgoCD : %v", err))
		return
	}

	audit.Log(r, "update-argocd-version", "", "global", "version="+r.FormValue("version"))
	h.redirectWithFlash(w, "/", "success", fmt.Sprintf("ArgoCD mis a jour sur preprod. MR prod : %s", mrURL))
}

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

// ListApps returns an HTMX fragment listing ArgoCD Applications.
// It queries Application objects directly from the vcluster API (source of truth).
// Falls back to the app-manifests GitLab repo if the vcluster is unreachable.
func (h *Handlers) ListApps(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	// List target vclusters (ArgoCD enabled, not the source) — needed for migration
	vclusters, _ := h.parser.ListVClusters(env)
	var targetVClusters []string
	for _, vc := range vclusters {
		if vc.ArgoCD && vc.Name != name {
			targetVClusters = append(targetVClusters, vc.Name)
		}
	}

	renderData := map[string]interface{}{
		"SourceVCluster":  name,
		"Env":             env,
		"User":            h.getUser(r),
		"TargetVClusters": targetVClusters,
	}

	// Try to read Application objects directly from the vcluster
	k8s := h.k8sForEnv(env)
	if k8s != nil {
		apps, err := k8s.ListVClusterArgoApps(r.Context(), name)
		if err == nil {
			// Annotate with migration state
			for i := range apps {
				if label := h.getMigrationLabel(env, name, apps[i].Name); label != "" {
					apps[i].Migrating = true
					apps[i].MigratingLabel = label
				}
			}
			renderData["Apps"] = apps
			renderData["AppsSource"] = "live"
			h.renderPartial(w, "apps_list.html", renderData)
			return
		}
		slog.Warn("ListApps: vcluster API unavailable, falling back to repo", "vcluster", name, "err", err)
	}

	// Fallback: read from app-manifests GitLab repo
	if h.gitlab == nil {
		h.renderPartial(w, "apps_list.html", renderData)
		return
	}

	branch := "preprod"
	if env == "prod" {
		branch = "master"
	}

	files, err := h.gitlab.ListAppManifestFiles(name, branch)
	if err != nil {
		slog.Error("ListApps: list files failed", "vcluster", name, "err", err)
		h.renderPartial(w, "apps_list.html", renderData)
		return
	}

	var apps []models.ArgoApp
	for _, filePath := range files {
		content, err := h.gitlab.GetAppManifestFile(name, branch, filePath)
		if err != nil {
			slog.Warn("ListApps: get file failed", "path", filePath, "err", err)
			continue
		}
		apps = append(apps, gitops.ParseArgoApps(filePath, content)...)
	}

	for i := range apps {
		if label := h.getMigrationLabel(env, name, apps[i].Name); label != "" {
			apps[i].Migrating = true
			apps[i].MigratingLabel = label
		}
	}

	renderData["Apps"] = apps
	renderData["AppsSource"] = "repo"
	h.renderPartial(w, "apps_list.html", renderData)
}

// MigrateApp copies an ArgoCD Application from one vcluster's app-manifests to another (admin only).
func (h *Handlers) MigrateApp(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	sourceName := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	appName := r.FormValue("app_name")
	filePath := r.FormValue("app_file_path")
	targetName := r.FormValue("target_vcluster")
	deleteSource := r.FormValue("delete_source") != ""

	if targetName == "" || targetName == sourceName {
		h.renderToast(w, "error", "Vcluster cible invalide")
		return
	}
	if filePath == "" {
		h.renderToast(w, "error", "Chemin du fichier manquant")
		return
	}

	if h.gitlab == nil {
		h.renderToast(w, "error", "GitLab client non disponible")
		return
	}

	branch := "preprod"
	if env == "prod" {
		branch = "master"
	}

	// Determine which files to migrate: all files in the same directory as the Application manifest.
	dir := path.Dir(filePath)
	allFiles, err := h.gitlab.ListAppManifestFiles(sourceName, branch)
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur liste fichiers source : %v", err))
		return
	}

	var dirFiles []string
	if dir == "." {
		// Application at repo root: migrate only this file
		dirFiles = []string{filePath}
	} else {
		prefix := dir + "/"
		for _, f := range allFiles {
			if strings.HasPrefix(f, prefix) {
				dirFiles = append(dirFiles, f)
			}
		}
		if len(dirFiles) == 0 {
			dirFiles = []string{filePath}
		}
	}

	// List files already in target to determine create vs update per file
	existingInTarget := map[string]bool{}
	if targetFiles, err2 := h.gitlab.ListAppManifestFiles(targetName, branch); err2 == nil {
		for _, f := range targetFiles {
			existingInTarget[f] = true
		}
	}

	// Read source files and build per-file commit actions
	var commitActions []gitops.CommitAction
	for _, f := range dirFiles {
		content, err := h.gitlab.GetAppManifestFile(sourceName, branch, f)
		if err != nil {
			h.renderToast(w, "error", fmt.Sprintf("Erreur lecture %s : %v", f, err))
			return
		}
		action := "create"
		if existingInTarget[f] {
			action = "update"
		}
		commitActions = append(commitActions, gitops.CommitAction{Action: action, Path: f, Content: content})
	}

	// Commit to target in a single atomic commit
	commitMsg := fmt.Sprintf("feat: migrate app %s from %s (%d files)", appName, sourceName, len(dirFiles))
	if err = h.gitlab.CommitToAppManifests(targetName, branch, commitMsg, commitActions); err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur migration vers %s : %v", targetName, err))
		return
	}

	// Optionally delete all directory files from source
	if deleteSource {
		var delActions []gitops.CommitAction
		for _, f := range dirFiles {
			delActions = append(delActions, gitops.CommitAction{Action: "delete", Path: f})
		}
		delMsg := fmt.Sprintf("feat: remove migrated app %s (%d files)", appName, len(dirFiles))
		if err := h.gitlab.CommitToAppManifests(sourceName, branch, delMsg, delActions); err != nil {
			h.renderToast(w, "warning", fmt.Sprintf("App migree vers %s mais erreur suppression source : %v", targetName, err))
			return
		}
	}

	h.addMigration(env, sourceName, targetName, appName)
	h.renderToast(w, "success", fmt.Sprintf("App %s migree vers %s (%d fichiers)", appName, targetName, len(dirFiles)))
}

// ProtectionStatus returns an HTMX fragment with the current namespace-protection state.
func (h *Handlers) ProtectionStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderPartial(w, "protection_status.html", map[string]interface{}{
			"Enabled": false,
		})
		return
	}

	protected := k8s.GetNamespaceProtection(r.Context(), name)
	h.renderPartial(w, "protection_status.html", map[string]interface{}{
		"Enabled":   true,
		"Protected": protected,
		"Name":      name,
		"Env":       env,
		"User":      h.getUser(r),
	})
}

// EnableProtection adds the protect-deletion annotation on the vcluster namespace (admin only).
func (h *Handlers) EnableProtection(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderToast(w, "error", fmt.Sprintf("Client Kubernetes %s non disponible", env))
		return
	}

	if err := k8s.SetNamespaceProtection(r.Context(), name, true); err != nil {
		slog.Error("EnableProtection failed", "env", env, "vcluster", name, "err", err)
		h.renderToast(w, "error", fmt.Sprintf("Erreur activation protection : %v", err))
		return
	}

	audit.Log(r, "enable-protection", name, env)
	h.renderPartial(w, "protection_status.html", map[string]interface{}{
		"Enabled":   true,
		"Protected": true,
		"Name":      name,
		"Env":       env,
		"User":      h.getUser(r),
	})
}

// DisableProtection removes the protect-deletion annotation on the vcluster namespace (admin only).
func (h *Handlers) DisableProtection(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderToast(w, "error", fmt.Sprintf("Client Kubernetes %s non disponible", env))
		return
	}

	if err := k8s.SetNamespaceProtection(r.Context(), name, false); err != nil {
		slog.Error("DisableProtection failed", "env", env, "vcluster", name, "err", err)
		h.renderToast(w, "error", fmt.Sprintf("Erreur desactivation protection : %v", err))
		return
	}

	audit.Log(r, "disable-protection", name, env)
	h.renderPartial(w, "protection_status.html", map[string]interface{}{
		"Enabled":   true,
		"Protected": false,
		"Name":      name,
		"Env":       env,
		"User":      h.getUser(r),
	})
}

// VeleroBackupList returns an HTMX fragment listing Velero backups for a vcluster.
func (h *Handlers) VeleroBackupList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderPartial(w, "velero_backups.html", map[string]interface{}{
			"Error": "Client Kubernetes non configuré",
			"Name":  name,
			"Env":   env,
		})
		return
	}

	backups, err := k8s.ListVeleroBackups(r.Context(), name, h.cfg.VeleroNamespace)
	if err != nil {
		h.renderPartial(w, "velero_backups.html", map[string]interface{}{
			"Error": err.Error(),
			"Name":  name,
			"Env":   env,
		})
		return
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].StartTime > backups[j].StartTime
	})

	// Also fetch active (non-terminal) restores so their polling survives page refresh
	activeRestores, _ := k8s.ListActiveVeleroRestores(r.Context(), name, h.cfg.VeleroNamespace)

	h.renderPartial(w, "velero_backups.html", map[string]interface{}{
		"Backups":        backups,
		"ActiveRestores": activeRestores,
		"Name":           name,
		"Env":            env,
		"User":           h.getUser(r),
	})
}

// VeleroBackupContent fetches the content URL for a backup and proxies the JSON result.
func (h *Handlers) VeleroBackupContent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	backup := r.PathValue("backup")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      "Client Kubernetes non configuré",
			"BackupName": backup,
		})
		return
	}

	ctx := r.Context()
	downloadURL, err := k8s.GetBackupContentURL(ctx, backup, h.cfg.VeleroNamespace)
	if err != nil {
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      fmt.Sprintf("Impossible d'obtenir le contenu : %v", err),
			"BackupName": backup,
		})
		return
	}

	// Fetch the JSON from the presigned URL
	resp, err := httpGetWithTimeout(downloadURL, 15*time.Second)
	if err != nil {
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      fmt.Sprintf("Téléchargement échoué : %v", err),
			"BackupName": backup,
		})
		return
	}
	defer resp.Body.Close()

	reader := io.Reader(resp.Body)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
				"Error":      fmt.Sprintf("Décompression échouée : %v", err),
				"BackupName": backup,
			})
			return
		}
		defer gz.Close()
		reader = gz
	}

	body, err := io.ReadAll(io.LimitReader(reader, 1<<20)) // 1MB max
	if err != nil {
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      fmt.Sprintf("Lecture du contenu échouée : %v", err),
			"BackupName": backup,
		})
		return
	}

	// Try gzip decompression even without Content-Encoding header (S3 may omit it)
	if len(body) > 1 && body[0] == 0x1f && body[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err == nil {
			defer gz.Close()
			if decompressed, err := io.ReadAll(io.LimitReader(gz, 1<<20)); err == nil {
				body = decompressed
			}
		}
	}

	// Pretty-print the JSON
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		pretty.Write(body)
	}

	h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
		"BackupName": backup,
		"Content":    pretty.String(),
		"Name":       name,
		"Env":        env,
	})
}

// CreateVeleroRestore initiates a Velero restore from a backup (admin only).
func (h *Handlers) CreateVeleroRestore(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}
	backupName := r.URL.Query().Get("backup")
	targetName := r.URL.Query().Get("target") // target vcluster name (empty = same vcluster)
	if backupName == "" {
		h.renderToast(w, "error", "Nom du backup manquant")
		return
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderToast(w, "error", "Client Kubernetes non configuré")
		return
	}

	sourceNS := "vcluster-" + name
	targetNS := "vcluster-" + name
	inPlace := targetName == "" || targetName == name
	if !inPlace {
		targetNS = "vcluster-" + targetName
	}

	// For in-place restores: suspend Flux, scale down vcluster, delete PVC so Velero can restore it.
	if inPlace {
		if err := k8s.SetFluxSuspend(r.Context(), name, true); err != nil {
			slog.Warn("could not suspend flux", "vcluster", name, "err", err)
		}
		if err := k8s.ScaleVClusterStatefulSet(r.Context(), name, 0); err != nil {
			slog.Warn("could not scale down vcluster", "vcluster", name, "err", err)
		} else {
			// Give the StatefulSet a moment to release the PVC before deleting it.
			time.Sleep(5 * time.Second)
			if err := k8s.DeleteVClusterPVC(r.Context(), name); err != nil {
				slog.Warn("could not delete PVC", "vcluster", name, "err", err)
			}
		}
	}

	restoreName, err := k8s.CreateVeleroRestore(r.Context(), backupName, sourceNS, targetNS, h.cfg.VeleroNamespace)
	if err != nil {
		// Resume Flux if restore creation failed (it will rescale the StatefulSet).
		if inPlace {
			if resumeErr := k8s.SetFluxSuspend(r.Context(), name, false); resumeErr != nil {
				slog.Warn("could not resume flux after failed restore", "vcluster", name, "err", resumeErr)
			}
		}
		h.renderToast(w, "error", fmt.Sprintf("Erreur création restore : %v", err))
		return
	}

	audit.Log(r, "velero-restore", name, env, "backup="+backupName, "target="+targetNS)
	h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
		"RestoreName": restoreName,
		"Phase":       "New",
		"Name":        name,
		"Env":         env,
		"BackupName":  backupName,
		"InPlace":     inPlace,
	})
}

// VeleroRestoreStatus returns the status of a Velero restore (HTMX polling).
func (h *Handlers) VeleroRestoreStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	restoreName := r.PathValue("restore")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}
	inPlace := r.URL.Query().Get("inplace") == "true"

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
			"Error":       "Client Kubernetes non configuré",
			"RestoreName": restoreName,
		})
		return
	}

	phase, err := k8s.GetRestoreStatus(r.Context(), restoreName, h.cfg.VeleroNamespace)
	if err != nil {
		h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
			"Error":       err.Error(),
			"RestoreName": restoreName,
			"Name":        name,
			"Env":         env,
			"InPlace":     inPlace,
		})
		return
	}

	// Resume Flux when restore is complete (in-place only).
	terminal := phase == "Completed" || phase == "Failed" || phase == "PartiallyFailed"
	if inPlace && terminal {
		if resumeErr := k8s.SetFluxSuspend(r.Context(), name, false); resumeErr != nil {
			slog.Warn("could not resume flux after restore", "vcluster", name, "err", resumeErr)
		}
	}

	h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
		"RestoreName": restoreName,
		"Phase":       phase,
		"Name":        name,
		"Env":         env,
		"InPlace":     inPlace,
	})
}

// TriggerVeleroBackup creates an on-demand Velero backup for a vcluster (admin only).
func (h *Handlers) TriggerVeleroBackup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderToast(w, "error", "Client Kubernetes non configuré")
		return
	}

	backupName, err := k8s.CreateVeleroBackup(r.Context(), name, h.cfg.VeleroNamespace, h.cfg.VeleroDefaultTTL, "")
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur création backup : %v", err))
		return
	}

	audit.Log(r, "velero-backup-manual", name, env, "backup="+backupName)

	// Return a toast + trigger refresh of backup list
	w.Header().Set("HX-Trigger", `{"veleroBackupsRefresh": true}`)
	h.renderToast(w, "success", fmt.Sprintf("Backup déclenché : %s", backupName))
}

// DeleteVeleroBackup deletes a Velero backup (admin only, Failed/PartiallyFailed).
func (h *Handlers) DeleteVeleroBackup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	backup := r.PathValue("backup")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderToast(w, "error", "Client Kubernetes non configuré")
		return
	}

	if err := k8s.DeleteVeleroBackup(r.Context(), backup, h.cfg.VeleroNamespace); err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur suppression backup : %v", err))
		return
	}

	audit.Log(r, "velero-backup-delete", name, env, "backup="+backup)
	w.Header().Set("HX-Trigger", `{"veleroBackupsRefresh": true}`)
	h.renderToast(w, "success", fmt.Sprintf("Backup %s supprimé", backup))
}

// httpGetWithTimeout performs a GET request with a timeout.
func httpGetWithTimeout(url string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	return client.Get(url) //nolint:noctx
}

// renameKubeconfig replaces generic cluster/context/user names with the vcluster name.
func renameKubeconfig(data []byte, name, env string) []byte {
	var kc map[string]interface{}
	if err := yaml.Unmarshal(data, &kc); err != nil {
		slog.Warn("could not parse kubeconfig for renaming", "err", err)
		return data
	}

	contextName := fmt.Sprintf("vcluster-%s-%s", name, env)

	// Rename clusters
	if clusters, ok := kc["clusters"].([]interface{}); ok {
		for _, c := range clusters {
			if cm, ok := c.(map[string]interface{}); ok {
				cm["name"] = contextName
			}
		}
	}

	// Rename users
	if users, ok := kc["users"].([]interface{}); ok {
		for _, u := range users {
			if um, ok := u.(map[string]interface{}); ok {
				um["name"] = contextName
			}
		}
	}

	// Rename contexts and update references
	if contexts, ok := kc["contexts"].([]interface{}); ok {
		for _, c := range contexts {
			if cm, ok := c.(map[string]interface{}); ok {
				cm["name"] = contextName
				if ctx, ok := cm["context"].(map[string]interface{}); ok {
					ctx["cluster"] = contextName
					ctx["user"] = contextName
				}
			}
		}
	}

	kc["current-context"] = contextName

	out, err := yaml.Marshal(kc)
	if err != nil {
		slog.Warn("could not marshal kubeconfig after renaming", "err", err)
		return data
	}
	return out
}
