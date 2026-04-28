package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

var nameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

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

// Create handles vcluster creation via GitOps.
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

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
	scope := r.FormValue("scope") // "preprod" or "both"
	if scope == "" {
		scope = "both"
	}

	// Validation
	if !nameRegex.MatchString(req.Name) {
		h.renderToast(w, "error", "Nom invalide : doit commencer par une lettre, uniquement [a-z0-9-]")
		return
	}
	if scope == "prod" {
		if h.parser.Exists(ctx, "prod", req.Name) {
			h.renderToast(w, "error", fmt.Sprintf("Le vcluster '%s' existe deja en prod", req.Name))
			return
		}
	} else {
		if h.parser.Exists(ctx, "preprod", req.Name) {
			h.renderToast(w, "error", fmt.Sprintf("Le vcluster '%s' existe deja", req.Name))
			return
		}
	}

	var warnings []string

	// 1. Commit preprod files (if scope includes preprod)
	if scope == "preprod" || scope == "both" {
		preprodFiles := h.generator.GenerateVCluster(req, "preprod")
		var preprodActions []gitops.CommitAction
		for _, f := range preprodFiles {
			preprodActions = append(preprodActions, gitops.CommitAction{
				Action:  "create",
				Path:    f.Path,
				Content: f.Content,
			})
		}
		kustAction, err := h.kustomizationAction(ctx, "preprod", "preprod", req.Name, true)
		if err != nil {
			slog.Warn("could not update kustomization.yaml", "env", "preprod", "err", err)
		} else {
			preprodActions = append(preprodActions, kustAction)
		}
		if err := h.gitlab.Commit(ctx, "preprod", fmt.Sprintf("feat: add vcluster %s", req.Name), preprodActions); err != nil {
			slog.Error("GitLab commit failed", "vcluster", req.Name, "err", err)
			h.renderToast(w, "error", "Erreur lors du commit GitLab : "+err.Error())
			return
		}
	}

	// 2. Commit prod files (if scope includes prod)
	if scope == "prod" || scope == "both" {
		var prodActions []gitops.CommitAction
		for _, f := range h.generator.GenerateVCluster(req, "prod") {
			prodActions = append(prodActions, gitops.CommitAction{
				Action:  "create",
				Path:    f.Path,
				Content: f.Content,
			})
		}
		kustAction, err := h.kustomizationAction(ctx, "prod", "preprod", req.Name, true)
		if err != nil {
			slog.Warn("could not read prod kustomization.yaml", "err", err)
		} else {
			prodActions = append(prodActions, kustAction)
		}

		if scope == "prod" {
			// Prod-only: create a MR so the change goes through review before reaching master
			mrURL, err := h.commitProdMRActions(
				ctx,
				fmt.Sprintf("feat: add vcluster %s (prod)", req.Name),
				fmt.Sprintf("Ajout du vcluster **%s** en production.\n\nCréé automatiquement par vcluster-manager.", req.Name),
				prodActions,
			)
			if err != nil {
				slog.Error("MR creation failed for prod-only vcluster", "vcluster", req.Name, "err", err)
				warnings = append(warnings, "Erreur création MR prod : "+err.Error())
			} else {
				slog.Info("MR created for prod-only vcluster", "vcluster", req.Name, "url", mrURL)
			}
		} else {
			// Both: commit prod files on preprod branch then create/get the MR preprod→master
			mrURL, err := h.commitProdMRActions(
				ctx,
				fmt.Sprintf("feat: add vcluster %s (prod)", req.Name),
				fmt.Sprintf("Ajout du vcluster **%s** en production.\n\nCréé automatiquement par vcluster-manager.", req.Name),
				prodActions,
			)
			if err != nil {
				slog.Error("MR creation failed for vcluster", "vcluster", req.Name, "err", err)
				warnings = append(warnings, "Erreur création MR prod : "+err.Error())
			} else {
				slog.Info("MR created/found for vcluster", "vcluster", req.Name, "url", mrURL)
			}
		}
	}

	// 3. Create GitLab repo for ArgoCD
	if req.ArgoCD {
		if _, err := h.gitlab.CreateAppManifestsRepo(req.Name); err != nil {
			slog.Error("GitLab repo creation failed", "vcluster", req.Name, "err", err)
			warnings = append(warnings, "Erreur repo GitLab : "+err.Error())
		}
	}

	// 4. Create Keycloak clients
	if req.ArgoCD {
		if h.keycloak != nil {
			if err := h.keycloak.CreateArgoCDClients(req.Name, scope); err != nil {
				slog.Error("Keycloak client creation failed", "vcluster", req.Name, "err", err)
				warnings = append(warnings, "Erreur Keycloak : "+err.Error())
			} else {
				slog.Info("Keycloak OIDC clients created", "vcluster", req.Name)
			}
		} else {
			slog.Warn("Keycloak not configured, skipping OIDC client creation", "vcluster", req.Name)
			warnings = append(warnings, "Keycloak non configure : le client OIDC d'ArgoCD ne sera pas cree")
		}
	}

	// 5. Vault Kubernetes auth backend setup (async — waits for vault-webhook to be deployed)
	if h.vault != nil {
		var envs []string
		if scope == "preprod" || scope == "both" {
			envs = append(envs, "preprod")
		}
		if scope == "prod" || scope == "both" {
			envs = append(envs, "prod")
		}
		for _, env := range envs {
			env := env
			go h.setupVaultAuthWhenReady(req.Name, env)
		}
	}

	audit.Log(r, "create", req.Name, scope)
	// Always redirect to dashboard
	msg := fmt.Sprintf("vcluster %s créé avec succès", req.Name)
	if len(warnings) > 0 {
		msg += " (warnings : " + strings.Join(warnings, " ; ") + ")"
	}
	h.redirectWithFlash(w, "/", "success", msg)
}

// setupVaultAuthWhenReady waits for the vault-webhook Kustomization to be Ready, then
// configures the Vault Kubernetes auth backend for the given vcluster and environment.
// Runs as a goroutine — errors are only logged.
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
	ctx := r.Context()
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	vc, err := h.parser.ParseVCluster(ctx, env, name)
	if err != nil {
		http.Error(w, "VCluster not found: "+err.Error(), http.StatusNotFound)
		return
	}

	var apiHost, argoURL string
	if env == "preprod" {
		apiHost = name + ".api." + h.cfg.BaseDomainPreprod
		if vc.ArgoCD {
			argoURL = "https://argocd." + name + "." + h.cfg.BaseDomainPreprod
		}
	} else {
		apiHost = name + ".api." + h.cfg.BaseDomainProd
		if vc.ArgoCD {
			argoURL = "https://argocd." + name + "." + h.cfg.BaseDomainProd
		}
	}

	// Check if app-manifests repo exists (for ArgoCD vclusters)
	appManifestsExists := false
	if vc.ArgoCD && h.gitlab != nil {
		appManifestsExists = h.gitlab.AppManifestsRepoExists(name)
	}

	// Check if this is a pending prod vcluster (not yet on master)
	isPending := env == "prod" && h.isPendingProd(ctx, name)
	// Deployed prod vclusters are read-only (modifications go through preprod editing)
	prodDeployed := env == "prod" && !isPending

	data := map[string]interface{}{
		"VCluster":           vc,
		"Env":                env,
		"EnvLabel":           h.cfg.ClusterLabel(env),
		"APIHost":            apiHost,
		"ArgoURL":            argoURL,
		"AppManifestsExists": appManifestsExists,
		"Pending":            isPending,
		"ProdDeployed":       prodDeployed,
		"User":               h.getUser(r),
	}

	// For all prod vclusters, fetch the open MR URL
	if env == "prod" && h.gitlab != nil {
		mrURL, mrChangedNames, _ := h.gitlab.GetOpenPreprodMRInfo()
		data["PendingMRURL"] = mrURL
		if isPending {
			// Pending: vcluster exists only on preprod, MR will deploy it
			data["HasPendingMRChange"] = mrURL != ""
		} else {
			// Deployed: check if the MR touches this specific vcluster
			data["HasPendingMRChange"] = mrChangedNames[name]
		}
	}

	// Rancher pairing status (if enabled for env, non-pending)
	data["RancherEnabled"] = h.rancher != nil && h.cfg.RancherEnabledForEnv(env)
	if h.rancher != nil && h.cfg.RancherEnabledForEnv(env) && !isPending {
		info, found, err := h.rancher.FindClusterByName(name)
		if err != nil {
			slog.Warn("Rancher lookup failed", "vcluster", name, "err", err)
		}
		data["RancherPaired"] = found && info.State == "active"
	}

	if h.ghReleases != nil {
		if versions, err := h.ghReleases.GetAvailableK8sVersions(); err == nil {
			data["K8sVersions"] = versions
		}
		if versions, err := h.ghReleases.GetAvailableArgoCDVersions(); err == nil {
			data["ArgoCDVersions"] = versions
		}
	}

	data["DefaultVeleroTTL"] = h.cfg.VeleroDefaultTTL
	ttlText := ttlToText(vc.Velero.TTL)
	if ttlText == "" {
		ttlText = ttlToText(h.cfg.VeleroDefaultTTL)
	}
	data["VeleroTTLText"] = ttlText

	h.render(w, "vcluster_detail.html", data)
}

// DeleteConfirm shows the deletion confirmation page.
func (h *Handlers) DeleteConfirm(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	// Get vcluster details for the targeted env
	vc, _ := h.parser.ParseVCluster(ctx, env, name)

	// Check if a counterpart exists in the other env
	var counterpartPath string
	if env == "prod" {
		counterpartPath = fmt.Sprintf("%s/preprod/vclusters/%s", h.cfg.FluxprodClustersPath, name)
	} else {
		counterpartPath = fmt.Sprintf("%s/prod/vclusters/%s", h.cfg.FluxprodClustersPath, name)
	}
	counterpartFiles, _ := h.gitlab.ListFiles(ctx, "preprod", counterpartPath)
	hasCounterpart := len(counterpartFiles) > 0

	// Check if namespace-protection is active (warn the user it will be auto-disabled)
	protectionEnabled := false
	if k8s := h.k8sForEnv(env); k8s != nil {
		protectionEnabled = k8s.GetNamespaceProtection(r.Context(), name)
	}

	data := map[string]interface{}{
		"Name":              name,
		"Env":               env,
		"HasCounterpart":    hasCounterpart,
		"ProtectionEnabled": protectionEnabled,
		"User":              h.getUser(r),
	}
	if vc != nil {
		data["VCluster"] = vc
	}

	h.render(w, "vcluster_delete.html", data)
}

// Delete handles vcluster deletion via GitOps.
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
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

	env := r.FormValue("env")
	if env == "" {
		env = "preprod"
	}
	deleteCounterpart := r.FormValue("delete_counterpart") == "on"
	deleteGitlab := r.FormValue("delete_gitlab") == "on"
	deleteKeycloak := h.keycloak != nil // always delete OIDC clients when Keycloak is configured

	deletePreprod := env == "preprod" || (env == "prod" && deleteCounterpart)
	deleteProd := env == "prod" || (env == "preprod" && deleteCounterpart)

	// Rancher must be unpaired BEFORE the vcluster is deleted (the cleanup job runs inside
	// the vcluster; if the vcluster is destroyed first the job never runs).
	if h.rancher != nil {
		for _, e := range []string{"preprod", "prod"} {
			if (e == "preprod" && !deletePreprod) || (e == "prod" && !deleteProd) {
				continue
			}
			if !h.cfg.RancherEnabledForEnv(e) {
				continue
			}
			if h.cfg.IsCleaning(name, e) {
				h.renderToast(w, "error", fmt.Sprintf("Nettoyage Rancher en cours pour %s (%s) — attendez la fin avant de supprimer", name, e))
				return
			}
			info, found, err := h.rancher.FindClusterByName(name)
			if err != nil || !found {
				continue
			}
			// Still paired: auto-unpair then delete in background
			k8s := h.k8sForEnv(e)
			if err := h.rancher.DeleteCluster(info.ID); err != nil {
				h.renderToast(w, "error", fmt.Sprintf("Erreur dépairage Rancher : %v", err))
				return
			}
			slog.Info("delete: Rancher cluster deleted, launching cleanup then deletion", "vcluster", name, "rancher_id", info.ID)
			h.cfg.AddCleaning(name, e, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak)
			go h.runCleanupAndDelete(name, e, k8s, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak)
			h.redirectWithFlash(w, "/", "info", fmt.Sprintf("Dépairage Rancher en cours — la suppression de %s sera lancée automatiquement", name))
			return
		}
	}

	audit.Log(r, "delete", name, env)
	// performDeletion goes through GitOps and must complete even if the user closes the
	// tab; detach from r.Context() and use a fresh background context for the chain.
	h.performDeletion(context.Background(), name, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak)
	var msg string
	if deletePreprod && deleteProd {
		msg = fmt.Sprintf("vcluster %s supprimé", name)
	} else if deleteProd {
		msg = fmt.Sprintf("vcluster %s (prod) supprimé", name)
	} else {
		msg = fmt.Sprintf("vcluster %s (preprod) supprimé", name)
	}
	h.redirectWithFlash(w, "/", "success", msg)
}

// runCleanupAndDelete runs the rancher-cleanup job then calls performDeletion.
// It is used both inline (initial delete request) and by startCleaningReconciler (restart recovery).
func (h *Handlers) runCleanupAndDelete(name, env string, k8s interface {
	ApplyManifestToVClusterViaPortForward(context.Context, string, []byte) error
	WaitForJobComplete(context.Context, string, string, string, time.Duration) error
}, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak bool) {
	ctx := context.Background()
	if k8s != nil {
		if err := k8s.ApplyManifestToVClusterViaPortForward(ctx, name, []byte(rancherCleanupManifest)); err != nil {
			slog.Warn("delete: rancher-cleanup deploy failed", "vcluster", name, "err", err)
		} else if err := k8s.WaitForJobComplete(ctx, name, "rancher-cleanup", "kube-system", 10*time.Minute); err != nil {
			slog.Warn("delete: rancher-cleanup job did not complete", "vcluster", name, "err", err)
		}
	}
	h.cfg.RemoveCleaning(name, env)
	h.performDeletion(ctx, name, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak)
}

// startCleaningReconciler runs at startup: for every active cleaning entry in cleaning.json,
// re-launches runCleanupAndDelete to resume any operation interrupted by a restart.
func (h *Handlers) startCleaningReconciler() {
	entries := h.cfg.ListCleaning()
	if len(entries) == 0 {
		return
	}
	for _, entry := range entries {
		entry := entry
		slog.Info("cleaning startup: resuming cleanup+deletion", "vcluster", entry.Name, "env", entry.Env)
		k8s := h.k8sForEnv(entry.Env)
		go h.runCleanupAndDelete(entry.Name, entry.Env, k8s,
			entry.DeletePreprod, entry.DeleteProd, entry.DeleteGitlab, entry.DeleteKeycloak)
	}
}

// performDeletion executes the GitOps deletion steps (K8s cleanup, GitLab commits, Keycloak, Vault).
// It is called either inline (when not Rancher-paired) or from a goroutine after Rancher cleanup.
func (h *Handlers) performDeletion(ctx context.Context, name string, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak bool) {
	// Cleanup K8s finalizers + disable namespace-protection before GitOps deletion
	for _, e := range []string{"preprod", "prod"} {
		if (e == "preprod" && !deletePreprod) || (e == "prod" && !deleteProd) {
			continue
		}
		if k8s := h.k8sForEnv(e); k8s != nil {
			// Disable namespace-protection so FluxCD can delete the namespace cleanly
			if k8s.GetNamespaceProtection(ctx, name) {
				if err := k8s.SetNamespaceProtection(ctx, name, false); err != nil {
					slog.Warn("disabling namespace-protection failed", "vcluster", name, "env", e, "err", err)
				} else {
					slog.Info("delete: namespace-protection disabled", "vcluster", name, "env", e)
				}
			}
			if err := k8s.CleanupNamespace(ctx, name); err != nil {
				slog.Warn("K8s cleanup failed", "vcluster", name, "env", e, "err", err)
			}
		}
	}

	// 1. Delete preprod files + update kustomization.yaml on preprod branch
	if deletePreprod {
		preprodPath := fmt.Sprintf("%s/preprod/vclusters/%s", h.cfg.FluxprodClustersPath, name)
		preprodFiles, err := h.gitlab.ListFiles(ctx, "preprod", preprodPath)
		if err != nil {
			slog.Error("error listing preprod files", "vcluster", name, "err", err)
		}
		var preprodActions []gitops.CommitAction
		for _, f := range preprodFiles {
			preprodActions = append(preprodActions, gitops.CommitAction{
				Action: "delete",
				Path:   f,
			})
		}
		kustAction, err := h.kustomizationAction(ctx, "preprod", "preprod", name, false)
		if err != nil {
			slog.Warn("could not update kustomization.yaml", "env", "preprod", "err", err)
		} else {
			preprodActions = append(preprodActions, kustAction)
		}
		if len(preprodActions) > 0 {
			if err := h.gitlab.Commit(ctx, "preprod", fmt.Sprintf("feat: remove vcluster %s", name), preprodActions); err != nil {
				slog.Error("error committing preprod deletion", "vcluster", name, "err", err)
				return
			}
			h.cfg.AddDeleting(name, "preprod", "")
			go h.sendNotification(fmt.Sprintf("Suppression du vcluster *%s* (preprod) en cours...", name))
		}
	}

	// 2. Handle prod deletion
	if deleteProd {
		prodPath := fmt.Sprintf("%s/prod/vclusters/%s", h.cfg.FluxprodClustersPath, name)
		prodFiles, err := h.gitlab.ListFiles(ctx, "preprod", prodPath)
		if err != nil {
			slog.Error("error listing prod files", "vcluster", name, "err", err)
		}
		if len(prodFiles) > 0 {
			isPending := h.isPendingProd(ctx, name)
			var prodActions []gitops.CommitAction
			for _, f := range prodFiles {
				prodActions = append(prodActions, gitops.CommitAction{
					Action: "delete",
					Path:   f,
				})
			}
			kustAction, err := h.kustomizationAction(ctx, "prod", "preprod", name, false)
			if err != nil {
				slog.Warn("could not update prod kustomization.yaml", "err", err)
			} else {
				prodActions = append(prodActions, kustAction)
			}

			if isPending {
				// Pending prod: delete directly on preprod branch (no MR, no HelmRelease to wait for)
				if err := h.gitlab.Commit(ctx, "preprod", fmt.Sprintf("feat: remove vcluster %s (prod)", name), prodActions); err != nil {
					slog.Error("error deleting pending prod files", "vcluster", name, "err", err)
				}
				// No AddDeleting: no K8s HelmRelease exists for pending vclusters
			} else {
				// Deployed prod: create MR for deletion
				mrURL, err := h.commitProdMRActions(
					ctx,
					fmt.Sprintf("feat: remove vcluster %s", name),
					fmt.Sprintf("Suppression du vcluster **%s** en production.\n\nCréé automatiquement par vcluster-manager.", name),
					prodActions,
				)
				if err != nil {
					slog.Error("error creating MR for prod deletion", "vcluster", name, "err", err)
				} else {
					slog.Info("MR created for prod deletion", "vcluster", name, "url", mrURL)
					h.cfg.AddDeleting(name, "prod", mrURL)
				}
			}
		}
	}

	if deleteGitlab {
		if err := h.gitlab.DeleteProject(name); err != nil {
			slog.Error("error deleting GitLab repo", "vcluster", name, "err", err)
		}
	}

	if deleteKeycloak && h.keycloak != nil {
		if err := h.keycloak.DeleteArgoCDClients(name); err != nil {
			slog.Error("error deleting Keycloak clients", "vcluster", name, "err", err)
		}
	}

	// Cleanup Vault Kubernetes auth backends
	if h.vault != nil {
		if deletePreprod {
			if err := h.vault.DisableAuth(context.Background(), "kubernetes-vcluster-"+name+"-preprod"); err != nil {
				slog.Warn("vault cleanup failed", "env", "preprod", "vcluster", name, "err", err)
			}
		}
		if deleteProd {
			if err := h.vault.DisableAuth(context.Background(), "kubernetes-vcluster-"+name+"-prod"); err != nil {
				slog.Warn("vault cleanup failed", "env", "prod", "vcluster", name, "err", err)
			}
		}
	}

}

// commitProdMRActions commits prod file changes to the preprod branch (source of truth),
// then gets or creates a single MR preprod→master to promote all pending prod changes.
func (h *Handlers) commitProdMRActions(ctx context.Context, commitMsg, mrDescription string, actions []gitops.CommitAction) (string, error) {
	// 1. Commit to preprod (source of truth for both envs)
	if err := h.gitlab.Commit(ctx, "preprod", commitMsg, actions); err != nil {
		return "", fmt.Errorf("committing to preprod: %w", err)
	}

	// 2. Get or create the standing MR preprod→master
	// preprod is the source of truth; master mirrors it once the MR is merged.
	// The description explains the diff structure (preprod files + prod files).
	mrNote := "Promotion des changements de preprod vers la production.\n\n" +
		"Créé automatiquement par vcluster-manager.\n\n---\n\n" +
		"> ℹ️ **Note sur le diff** : Ce MR contient des fichiers sous `clusters/preprod/` **et** `clusters/prod/`.\n" +
		"> Seuls les fichiers sous **`clusters/prod/`** ont un impact sur la production.\n" +
		"> Les fichiers `clusters/preprod/` sont présents car la branche **preprod est la source de vérité** pour les deux environnements."
	mrURL, err := h.gitlab.GetOrCreateMergeRequest(
		"preprod", "master",
		"feat: promote preprod to prod",
		mrNote,
	)
	if err != nil {
		return "", fmt.Errorf("creating MR: %w", err)
	}

	return mrURL, nil
}

// kustomizationAction reads the cluster kustomization.yaml and returns a CommitAction to add/remove a vcluster entry.
func (h *Handlers) kustomizationAction(ctx context.Context, env, branch, name string, add bool) (gitops.CommitAction, error) {
	kustPath := fmt.Sprintf("%s/%s/kustomization.yaml", h.cfg.FluxprodClustersPath, env)
	content, err := h.gitlab.GetFile(ctx, branch, kustPath)
	if err != nil {
		return gitops.CommitAction{}, fmt.Errorf("reading %s: %w", kustPath, err)
	}
	updated := gitops.UpdateKustomization(content, name, add)
	return gitops.CommitAction{
		Action:  "update",
		Path:    kustPath,
		Content: updated,
	}, nil
}

// isPendingProd returns true if a prod vcluster exists on preprod but not yet on master.
func (h *Handlers) isPendingProd(ctx context.Context, name string) bool {
	for _, n := range h.parser.ListVClusterNamesOnBranch(ctx, "master", "prod") {
		if n == name {
			return false
		}
	}
	return true
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
