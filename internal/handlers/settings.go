package handlers

import (
	"errors"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/service"
)

// UpdateSettings handles vcluster settings modification: parses the form here,
// delegates RBAC/validation/GitOps to service.UpdateSettings, and maps the
// typed result/errors back to the same toasts and redirects the inline
// handler used to render.
func (h *Handlers) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	in := service.UpdateSettingsInput{
		VeleroEnabled: r.FormValue("velero_enabled") == "on",
		VeleroHour:    r.FormValue("velero_hour"),
		VeleroTTL:     parseTTLText(r.FormValue("velero_ttl")),
		CPU:           r.FormValue("cpu"),
		Memory:        r.FormValue("memory"),
		Storage:       r.FormValue("storage"),
		NoQuotas:      r.FormValue("no_quotas") == "on",
		RBACGroups:    splitGroups(r.FormValue("rbac_groups"), h.cfg.DefaultRBACGroup),
		K8sVersion:    r.FormValue("k8s_version"),
		ArgoCDVersion: r.FormValue("argocd_version"),
		FluxCDEnabled: r.FormValue("fluxcd_enabled") == "on",
		FluxCDRepoURL: r.FormValue("fluxcd_repo_url"),
		FluxCDBranch:  r.FormValue("fluxcd_branch"),
		FluxCDPath:    r.FormValue("fluxcd_path"),
		ArgoCDToggle:  r.FormValue("argocd"), // "on", "off", or "" (not changing)
		FluxCDToggle:  r.FormValue("fluxcd"), // "on", "off", or "" (not changing)
		DeleteRepo:    r.FormValue("delete_repo") == "on",
	}

	res, err := h.svc.UpdateSettings(r.Context(), h.actor(r), name, r.URL.Query().Get("env"), in)
	if err != nil {
		h.handleUpdateSettingsError(w, err)
		return
	}

	h.redirectWithFlash(w, res.RedirectURL, res.FlashLevel, res.FlashMessage)
}

// handleUpdateSettingsError maps service.UpdateSettings' typed errors back to
// the exact toasts the inline handler used to render.
func (h *Handlers) handleUpdateSettingsError(w http.ResponseWriter, err error) {
	var notFoundErr *service.VClusterNotFoundError
	var commitErr *service.CommitError
	switch {
	case errors.Is(err, service.ErrForbidden):
		w.WriteHeader(http.StatusForbidden)
		h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
	case errors.As(err, &notFoundErr):
		h.renderToast(w, "error", "VCluster introuvable : "+notFoundErr.Err.Error())
	case errors.As(err, &commitErr):
		h.renderToast(w, "error", "Erreur commit : "+commitErr.Err.Error())
	default:
		// Field-validation errors (cpu/memory/storage/k8s_version/argocd_version/
		// fluxcd_*/velero_hour) come through here as plain "field : reason" errors.
		h.renderToast(w, "error", err.Error())
	}
}
