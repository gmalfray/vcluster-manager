package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/service"
	"gopkg.in/yaml.v3"
)

// RetryVaultSetup re-triggers the Vault auth backend setup for a given vcluster (admin only).
// Useful after a vcluster-manager restart or a previous setup failure.
func (h *Handlers) RetryVaultSetup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	name := r.PathValue("name")
	env := reqEnv(r)

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

	audit.Log(r, "vault-setup-retry", name, env)
	go h.setupVaultAuthWhenReady(name, env)
	h.renderToast(w, "success", fmt.Sprintf("Setup Vault relance pour %s (%s)", name, env))
}

// FluxSummaryFragment returns an HTMX fragment with HelmRelease counts for the dashboard Flux Status card.
func (h *Handlers) FluxSummaryFragment(w http.ResponseWriter, r *http.Request) {
	summary := h.svc.GetFluxSummary(r.Context())
	h.renderPartial(w, "flux_summary.html", map[string]interface{}{
		"Total":        summary.Total,
		"Ready":        summary.Ready,
		"PreprodTotal": summary.PreprodTotal,
		"PreprodReady": summary.PreprodReady,
		"ProdTotal":    summary.ProdTotal,
		"ProdReady":    summary.ProdReady,
	})
}

// StatusFragment returns an HTMX fragment with the vcluster status badge.
func (h *Handlers) StatusFragment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := reqEnv(r)

	// Handle deleting mode: check if HelmRelease still exists. This branch stays in
	// the web adapter — it drives HTMX-specific side effects (RemoveDeleting,
	// notifications, HX-Redirect) that don't belong in the read-only service.
	//
	// The ?deleting= flag is a rendering hint from the poller, never the authority:
	// the branch mutates the cluster (CleanupNamespace strips finalizers) and fires
	// a webhook, so it only runs for a vcluster the server itself recorded as being
	// deleted. Trusting the query string let any authenticated caller — a reader
	// included — trigger a finalizer cleanup on an arbitrary name.
	if deleting, _ := h.cfg.IsDeleting(name, env); deleting && r.URL.Query().Get("deleting") == "true" {
		k8s := h.k8sForEnv(env)
		if k8s != nil && !k8s.HelmReleaseExists(r.Context(), name) {
			// HelmRelease gone: remove any stuck FluxCD finalizers then cleanup
			if err := k8s.CleanupNamespace(r.Context(), name); err != nil {
				slog.Warn("namespace cleanup failed", "env", env, "vcluster", name, "err", err)
			}
			h.cfg.RemoveDeleting(name, env)
			go h.sendNotification(context.Background(), fmt.Sprintf("vcluster *%s* (%s) supprime avec succes.", name, env))
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

	// Normal path: the service resolves the real-time status.
	status, err := h.svc.GetStatus(r.Context(), name, env)
	if err != nil {
		if errors.Is(err, service.ErrK8sUnavailable) {
			h.renderPartial(w, "status_badge.html", map[string]interface{}{
				"HelmRelease":   "N/A",
				"Kustomization": "N/A",
			})
			return
		}
		h.renderPartial(w, "status_badge.html", map[string]interface{}{
			"HelmRelease":   "Error",
			"Kustomization": "Error",
		})
		return
	}

	configVersion := r.URL.Query().Get("configVersion")

	data := map[string]interface{}{
		"HelmRelease":    status.HelmRelease,
		"Kustomization":  status.Kustomization,
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
	if !service.ValidName(name) {
		http.Error(w, "Invalid vcluster name", http.StatusBadRequest)
		return
	}
	env := reqEnv(r)

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
	if _, err := w.Write(kubeconfig); err != nil {
		slog.Warn("kubeconfig write failed", "vcluster", name, "env", env, "err", err)
	}
}

// CreateAppManifestsRepo creates the app-manifests GitLab repo for a vcluster (admin only).
func (h *Handlers) CreateAppManifestsRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")

	if _, err := h.svc.CreateAppManifestsRepo(r.Context(), h.actor(r), name, env); err != nil {
		switch {
		case errors.Is(err, service.ErrForbidden):
			w.WriteHeader(http.StatusForbidden)
			h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
		case errors.Is(err, service.ErrAppGitLabUnavailable):
			h.renderToast(w, "error", "GitLab client non disponible")
		default:
			h.renderToast(w, "error", fmt.Sprintf("Erreur creation repo : %v", err))
		}
		return
	}

	h.redirectWithFlash(w, fmt.Sprintf("/vclusters/%s?env=%s", name, env), "success", fmt.Sprintf("Repo app-manifests-%s cree avec succes", name))
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
	audit.Log(r, "create-prod-mr", name, "prod", "mr_url="+mrURL)
	h.redirectWithFlash(w, fmt.Sprintf("/vclusters/%s?env=prod", name), "success", fmt.Sprintf("MR créée : %s", mrURL))
}

// QuotaForm returns the quota editing form fragment.
func (h *Handlers) QuotaForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := reqEnv(r)

	vc, err := h.svc.GetQuotaForm(r.Context(), env, name)
	if err != nil {
		http.Error(w, "VCluster not found", http.StatusNotFound)
		return
	}

	h.renderPartial(w, "quota_form.html", map[string]interface{}{
		"VCluster": vc,
		"Env":      env,
	})
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
