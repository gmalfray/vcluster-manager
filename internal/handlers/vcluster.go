package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// ListVClusters shows the vcluster list page.
func (h *Handlers) ListVClusters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	vclusters, err := h.parser.ListVClusters(ctx, env)
	if err != nil {
		http.Error(w, "Error listing vclusters: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build deleting names map for this env
	deletingNames := map[string]bool{}
	for _, de := range h.cfg.ListDeleting() {
		// Only show deleting status if it matches the current environment
		// - If deleting a preprod vcluster (or created via "both"), it appears in preprod list
		// - If deleting a prod vcluster (via MR), it appears in prod list
		if de.Env == env {
			deletingNames[de.Name] = true
		}
	}

	data := map[string]interface{}{
		"VClusters":     vclusters,
		"Env":           env,
		"EnvLabel":      h.cfg.ClusterLabel(env),
		"PreprodLabel":  h.cfg.ClusterLabel("preprod"),
		"ProdLabel":     h.cfg.ClusterLabel("prod"),
		"DeletingNames": deletingNames,
		"User":          h.getUser(r),
	}

	// Pass default K8s version so status badge can show it for vclusters without a specific version
	if h.helmUpdater != nil {
		branch := "preprod"
		if env == "prod" {
			branch = "master"
		}
		if k8s, err := h.helmUpdater.GetDefaultK8sVersion(ctx, branch); err == nil {
			data["DefaultK8sVersion"] = k8s
		}
	}

	// For prod, check which vclusters are actually deployed on master
	if env == "prod" {
		masterNames := map[string]bool{}
		for _, name := range h.parser.ListVClusterNamesOnBranch(ctx, "master", "prod") {
			masterNames[name] = true
		}
		pendingNames := map[string]bool{}
		for _, vc := range vclusters {
			if !masterNames[vc.Name] {
				pendingNames[vc.Name] = true
			}
		}
		data["PendingNames"] = pendingNames

		// Check for open preprod→master MR and which vclusters it touches
		mrURL, mrChangedNames, _ := h.gitlab.GetOpenPreprodMRInfo()
		data["MRChangedNames"] = mrChangedNames
		data["PendingMRURL"] = mrURL
	}

	h.render(w, "vcluster_list.html", data)
}

// CreateForm shows the creation form.
func (h *Handlers) CreateForm(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	usedSlots := h.parser.UsedVeleroSlots(r.Context(), "preprod")
	h.render(w, "vcluster_create.html", map[string]interface{}{
		"UsedSlots": usedSlots,
		"User":      h.getUser(r),
	})
}

// Create handles vcluster creation via GitOps. Field parsing and rendering
// stay here; the business logic (validation, fluxprod commits, side
// integrations) lives in service.Create — RBAC included, which is why there's
// no requireAdmin gate at the top: a non-admin request reaches the service
// and comes back as service.ErrForbidden, handled the same way below.
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	req := &models.CreateRequest{
		Name:          r.FormValue("name"),
		ArgoCD:        r.FormValue("argocd") == "on",
		RBACGroups:    splitGroups(r.FormValue("rbac_groups"), h.cfg.DefaultRBACGroup),
		VeleroEnabled: r.FormValue("velero_enabled") == "on",
		VeleroHour:    r.FormValue("velero_hour"),
		CPU:           r.FormValue("cpu"),
		Memory:        r.FormValue("memory"),
		Storage:       r.FormValue("storage"),
		NoQuotas:      r.FormValue("no_quotas") == "on",
		FluxCDEnabled: r.FormValue("fluxcd_enabled") == "on",
		FluxCDRepoURL: r.FormValue("fluxcd_repo_url"),
		FluxCDBranch:  r.FormValue("fluxcd_branch"),
		FluxCDPath:    r.FormValue("fluxcd_path"),
	}
	scope := r.FormValue("scope") // "preprod", "prod" or "both" ("" defaults to "both" in the service)

	res, err := h.svc.Create(r.Context(), h.actor(r), req, scope)
	if err != nil {
		h.handleCreateError(w, err)
		return
	}

	// Vault auth-backend setup is async and waits on the fresh vcluster's
	// vault-webhook to come up — the goroutine stays here, the service just
	// says which environments need it.
	for _, env := range res.VaultSetupEnvs {
		go h.setupVaultAuthWhenReady(req.Name, env)
	}

	msg := fmt.Sprintf("vcluster %s créé avec succès", res.Name)
	if len(res.Warnings) > 0 {
		msg += " (warnings : " + strings.Join(res.Warnings, " ; ") + ")"
	}
	h.redirectWithFlash(w, "/", "success", msg)
}

// handleCreateError maps service.Create's typed errors back to the exact
// toasts the inline handler used to render.
func (h *Handlers) handleCreateError(w http.ResponseWriter, err error) {
	var existsErr *service.ExistsError
	var commitErr *service.CommitError
	switch {
	case errors.Is(err, service.ErrForbidden):
		w.WriteHeader(http.StatusForbidden)
		h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
	case errors.Is(err, service.ErrInvalidName):
		h.renderToast(w, "error", "Nom invalide : doit commencer par une lettre, uniquement [a-z0-9-]")
	case errors.As(err, &existsErr):
		if existsErr.Env == "prod" {
			h.renderToast(w, "error", fmt.Sprintf("Le vcluster '%s' existe deja en prod", existsErr.Name))
		} else {
			h.renderToast(w, "error", fmt.Sprintf("Le vcluster '%s' existe deja", existsErr.Name))
		}
	case errors.As(err, &commitErr):
		h.renderToast(w, "error", "Erreur lors du commit GitLab : "+commitErr.Err.Error())
	default:
		// Field-validation errors (cpu/memory/storage/fluxcd_*/velero_hour) come
		// through here as plain "field : reason" errors.
		h.renderToast(w, "error", err.Error())
	}
}

// setupVaultAuthWhenReady waits for the vault-webhook Kustomization to be Ready, then
// configures the Vault Kubernetes auth backend for the given vcluster and environment.
// Runs as a goroutine — errors are only logged. Kept in the handler (not the service)
// because it's an async polling loop, launched by Create and by the vault reconciler
// in handlers.go.
func (h *Handlers) setupVaultAuthWhenReady(name, env string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		slog.Warn("vault: no k8s client configured, skipping Vault setup", "env", env, "vcluster", name)
		return
	}

	h.setVaultState(env, name, "waiting", "")
	slog.Info("vault: waiting for vault-webhook Kustomization to be Ready", "env", env, "vcluster", name)
	if err := k8s.WaitForVaultWebhookReady(ctx, name); err != nil {
		slog.Error("vault: vault-webhook not ready", "env", env, "vcluster", name, "err", err)
		h.setVaultState(env, name, "error", err.Error())
		return
	}

	h.setVaultState(env, name, "configuring", "")
	slog.Info("vault: generating reviewer token", "env", env, "vcluster", name)
	token, caCert, err := k8s.CreateVaultReviewerToken(ctx, name, 876000*time.Hour)
	if err != nil {
		slog.Error("vault: token generation failed", "env", env, "vcluster", name, "err", err)
		h.setVaultState(env, name, "error", err.Error())
		return
	}

	slog.Info("vault: configuring Vault backend", "env", env, "vcluster", name)
	domain := h.cfg.BaseDomainProd
	if env == "preprod" {
		domain = h.cfg.BaseDomainPreprod
	}
	apiHost := "https://" + name + ".api." + domain
	if err := h.vault.SetupVClusterAuth(ctx, name, env, apiHost, caCert, token); err != nil {
		slog.Error("vault: setup failed", "env", env, "vcluster", name, "err", err)
		h.setVaultState(env, name, "error", err.Error())
		return
	}
	h.setVaultState(env, name, "done", "")
	slog.Info("vault: backend configured successfully", "env", env, "vcluster", name)
}

// Detail shows a single vcluster's details.
func (h *Handlers) Detail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")

	detail, err := h.svc.GetVCluster(r.Context(), name, env)
	if err != nil {
		var notFound *service.VClusterNotFoundError
		if errors.As(err, &notFound) {
			http.Error(w, "VCluster not found: "+notFound.Err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, "VCluster not found: "+err.Error(), http.StatusNotFound)
		return
	}

	data := map[string]interface{}{
		"VCluster":           detail.VCluster,
		"Env":                detail.Env,
		"EnvLabel":           detail.EnvLabel,
		"APIHost":            detail.APIHost,
		"ArgoURL":            detail.ArgoURL,
		"AppManifestsExists": detail.AppManifestsExists,
		"Pending":            detail.Pending,
		"ProdDeployed":       detail.ProdDeployed,
		"User":               h.getUser(r),
		"PendingMRURL":       detail.PendingMRURL,
		"HasPendingMRChange": detail.HasPendingMRChange,
		"RancherEnabled":     detail.RancherEnabled,
		"RancherPaired":      detail.RancherPaired,
		"K8sVersions":        detail.K8sVersions,
		"ArgoCDVersions":     detail.ArgoCDVersions,
	}

	// TTL formatting stays here: it's display-only, the service hands over the
	// raw value.
	data["DefaultVeleroTTL"] = h.cfg.VeleroDefaultTTL
	ttlText := ttlToText(detail.VCluster.Velero.TTL)
	if ttlText == "" {
		ttlText = ttlToText(h.cfg.VeleroDefaultTTL)
	}
	data["VeleroTTLText"] = ttlText

	h.render(w, "vcluster_detail.html", data)
}

// DeleteConfirm shows the deletion confirmation page.
func (h *Handlers) DeleteConfirm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")

	confirm, err := h.svc.GetDeleteConfirm(r.Context(), h.actor(r), name, env)
	if err != nil {
		if errors.Is(err, service.ErrForbidden) {
			w.WriteHeader(http.StatusForbidden)
			h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
			return
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Name":              confirm.Name,
		"Env":               confirm.Env,
		"HasCounterpart":    confirm.HasCounterpart,
		"ProtectionEnabled": confirm.ProtectionEnabled,
		"User":              h.getUser(r),
	}
	if confirm.VCluster != nil {
		data["VCluster"] = confirm.VCluster
	}

	h.render(w, "vcluster_delete.html", data)
}

// Delete handles vcluster deletion via GitOps. Form parsing, the confirm_name
// guard and rendering stay here; the Rancher-first decision and the actual
// GitOps teardown live in service.Delete / service.PerformDeletion.
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	// Confirmation check
	if r.FormValue("confirm_name") != name {
		h.renderToast(w, "error", "Le nom ne correspond pas")
		return
	}

	in := service.DeleteInput{
		Env:               r.FormValue("env"),
		DeleteCounterpart: r.FormValue("delete_counterpart") == "on",
		DeleteGitlab:      r.FormValue("delete_gitlab") == "on",
	}

	res, err := h.svc.Delete(r.Context(), h.actor(r), name, in)
	if err != nil {
		h.handleDeleteError(w, err)
		return
	}

	if res.Async {
		// Still Rancher-paired: the service already unpaired it and recorded the
		// cleaning state. The wait-for-job-then-delete goroutine is an async
		// concern, so it's launched here, not inside the service.
		k8s := h.k8sForEnv(res.CleaningEnv)
		go h.runCleanupAndDelete(name, res.CleaningEnv, k8s, res.DeletePreprod, res.DeleteProd, res.DeleteGitlab, res.DeleteKeycloak)
		h.redirectWithFlash(w, "/", "info", fmt.Sprintf("Dépairage Rancher en cours — la suppression de %s sera lancée automatiquement", name))
		return
	}

	var msg string
	switch {
	case res.DeletePreprod && res.DeleteProd:
		msg = fmt.Sprintf("vcluster %s supprimé", name)
	case res.DeleteProd:
		msg = fmt.Sprintf("vcluster %s (prod) supprimé", name)
	default:
		msg = fmt.Sprintf("vcluster %s (preprod) supprimé", name)
	}
	h.redirectWithFlash(w, "/", "success", msg)
}

// handleDeleteError maps service.Delete's typed errors back to the exact
// toasts the inline handler used to render.
func (h *Handlers) handleDeleteError(w http.ResponseWriter, err error) {
	var cleaningErr *service.CleaningError
	var unpairErr *service.UnpairError
	switch {
	case errors.Is(err, service.ErrForbidden):
		w.WriteHeader(http.StatusForbidden)
		h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
	case errors.As(err, &cleaningErr):
		h.renderToast(w, "error", fmt.Sprintf("Nettoyage Rancher en cours pour %s (%s) — attendez la fin avant de supprimer", cleaningErr.Name, cleaningErr.Env))
	case errors.As(err, &unpairErr):
		h.renderToast(w, "error", fmt.Sprintf("Erreur dépairage Rancher : %v", unpairErr.Err))
	default:
		h.renderToast(w, "error", err.Error())
	}
}

// runCleanupAndDelete runs the rancher-cleanup job then calls PerformDeletion.
// It is used both inline (initial delete request) and by startCleaningReconciler
// (restart recovery). Stays in the handler: it's an async polling wait, not
// business logic.
func (h *Handlers) runCleanupAndDelete(name, env string, k8s interface {
	ApplyManifestToVClusterViaPortForward(context.Context, string, []byte) error
	WaitForJobComplete(context.Context, string, string, string, time.Duration) error
}, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak bool) {
	ctx := context.Background()
	if k8s != nil {
		if err := k8s.ApplyManifestToVClusterViaPortForward(ctx, name, []byte(service.RancherCleanupManifest)); err != nil {
			slog.Warn("delete: rancher-cleanup deploy failed", "vcluster", name, "err", err)
		} else if err := k8s.WaitForJobComplete(ctx, name, "rancher-cleanup", "kube-system", 10*time.Minute); err != nil {
			slog.Warn("delete: rancher-cleanup job did not complete", "vcluster", name, "err", err)
		}
	}
	h.cfg.RemoveCleaning(name, env)
	h.svc.PerformDeletion(ctx, name, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak)
}

// startCleaningReconciler runs at startup: for every active cleaning entry in cleaning.json,
// re-launches runCleanupAndDelete to resume any operation interrupted by a restart.
func (h *Handlers) startCleaningReconciler() {
	entries := h.cfg.ListCleaning()
	if len(entries) == 0 {
		return
	}
	for _, entry := range entries {
		slog.Info("cleaning startup: resuming cleanup+deletion", "vcluster", entry.Name, "env", entry.Env)
		k8s := h.k8sForEnv(entry.Env)
		go h.runCleanupAndDelete(entry.Name, entry.Env, k8s,
			entry.DeletePreprod, entry.DeleteProd, entry.DeleteGitlab, entry.DeleteKeycloak)
	}
}

func splitGroups(s, defaultGroup string) []string {
	var groups []string
	for _, g := range strings.Split(s, ",") {
		g = strings.TrimSpace(g)
		if g != "" {
			groups = append(groups, g)
		}
	}
	if len(groups) == 0 {
		if defaultGroup != "" {
			groups = []string{defaultGroup}
		} else {
			groups = []string{"admin"}
		}
	}
	return groups
}

func (h *Handlers) renderToast(w http.ResponseWriter, level, message string) {
	h.renderPartial(w, "toast.html", map[string]interface{}{
		"Level":   level,
		"Message": message,
	})
}
