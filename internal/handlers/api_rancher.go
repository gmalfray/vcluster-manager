package handlers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/service"
)

// PairRancher registers a vcluster in Rancher (prod only, admin only). It
// delegates to the service (which enforces admin RBAC, runs the async import
// and returns the immediate "pairing" state) and maps the typed domain errors
// back to the exact HTMX toasts the UI expected before the extraction.
func (h *Handlers) PairRancher(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.PairRancher(r.Context(), h.actor(r), r.PathValue("name"), r.URL.Query().Get("env"))
	if err != nil {
		h.renderRancherError(w, err, rancherReqEnv(r))
		return
	}
	h.renderRancher(w, st)
}

// UnpairRancher removes a vcluster from Rancher (prod only, admin only). It
// delegates to the service and maps typed errors to the same toasts as before.
func (h *Handlers) UnpairRancher(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.UnpairRancher(r.Context(), h.actor(r), r.PathValue("name"), r.URL.Query().Get("env"))
	if err != nil {
		h.renderRancherError(w, err, rancherReqEnv(r))
		return
	}
	h.renderRancher(w, st)
}

// RancherStatus returns an HTMX fragment with the current Rancher pairing status.
func (h *Handlers) RancherStatus(w http.ResponseWriter, r *http.Request) {
	st := h.svc.GetRancherStatus(r.Context(), r.PathValue("name"), r.URL.Query().Get("env"))
	h.renderRancher(w, st)
}

// rancherReqEnv resolves the request's env the same way the service does, so
// error toasts mentioning the environment stay identical to before.
func rancherReqEnv(r *http.Request) string {
	env := r.URL.Query().Get("env")
	if env == "" {
		return "preprod"
	}
	return env
}

// renderRancherError maps the rancher-domain service errors onto the exact
// same HTMX toasts (and HTTP statuses) the handler produced before the
// extraction.
func (h *Handlers) renderRancherError(w http.ResponseWriter, err error, env string) {
	var alreadyExists *service.AlreadyExistsError
	var opErr *service.RancherOpError
	switch {
	case errors.Is(err, service.ErrForbidden):
		w.WriteHeader(http.StatusForbidden)
		h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
	case errors.Is(err, service.ErrRancherNotConfigured):
		h.renderToast(w, "error", "Client Rancher non configure")
	case errors.Is(err, service.ErrRancherNotEnabled):
		h.renderToast(w, "error", fmt.Sprintf("Rancher n'est pas active pour l'environnement %s", env))
	case errors.Is(err, service.ErrCleaningInProgress):
		h.renderToast(w, "error", "Nettoyage Rancher en cours, veuillez patienter")
	case errors.Is(err, service.ErrRancherK8sProdUnavailable):
		h.renderToast(w, "error", "Client Kubernetes prod non disponible")
	case errors.Is(err, service.ErrAlreadyPaired):
		h.renderToast(w, "error", "Ce vcluster est déjà appairé dans Rancher (état: active). Désappairez-le d'abord.")
	case errors.As(err, &alreadyExists):
		h.renderToast(w, "error", fmt.Sprintf("Ce vcluster existe déjà dans Rancher (état: %s). Attendez ou désappairez-le d'abord.", alreadyExists.State))
	case errors.Is(err, service.ErrManualPairing):
		h.renderToast(w, "error", "Des agents Rancher sont déjà actifs dans ce vcluster (appairage manuel détecté). Désappairez-le d'abord.")
	case errors.As(err, &opErr):
		if opErr.Op == "delete" {
			h.renderToast(w, "error", fmt.Sprintf("Erreur suppression Rancher : %v", opErr.Err))
		} else {
			h.renderToast(w, "error", fmt.Sprintf("Erreur recherche Rancher : %v", opErr.Err))
		}
	default:
		h.renderToast(w, "error", fmt.Sprintf("Erreur Rancher : %v", err))
	}
}

// renderRancher renders the rancher_status.html HTMX fragment from a
// service.RancherStatus. When Rancher is disabled it emits the minimal
// fragment (Enabled:false only), exactly as before.
func (h *Handlers) renderRancher(w http.ResponseWriter, st service.RancherStatus) {
	if !st.Enabled {
		h.renderPartial(w, "rancher_status.html", map[string]interface{}{
			"Enabled": false,
		})
		return
	}
	h.renderPartial(w, "rancher_status.html", map[string]interface{}{
		"Enabled":        true,
		"Paired":         st.Paired,
		"Pairing":        st.Pairing,
		"Unknown":        st.Unknown,
		"ManuallyPaired": st.ManuallyPaired,
		"Cleaning":       st.Cleaning,
		"Name":           st.Name,
		"Env":            st.Env,
	})
}
