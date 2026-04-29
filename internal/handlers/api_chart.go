package handlers

import (
	"fmt"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/audit"
)

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

	mrURL, err := h.helmUpdater.UpdateChart(r.Context(), version)
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

	mrURL, err := h.helmUpdater.UpdateK8sVersion(r.Context(), version)
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

	mrURL, err := h.argocdUpdater.UpdateGlobalVersion(r.Context(), version)
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur lors de la mise a jour ArgoCD : %v", err))
		return
	}

	audit.Log(r, "update-argocd-version", "", "global", "version="+r.FormValue("version"))
	h.redirectWithFlash(w, "/", "success", fmt.Sprintf("ArgoCD mis a jour sur preprod. MR prod : %s", mrURL))
}
