package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/service"
)

// ProtectionStatus returns an HTMX fragment with the current namespace-protection state.
func (h *Handlers) ProtectionStatus(w http.ResponseWriter, r *http.Request) {
	st := h.svc.GetProtection(r.Context(), r.PathValue("name"), r.URL.Query().Get("env"))
	h.renderProtection(w, r, st)
}

// EnableProtection adds the protect-deletion annotation on the vcluster namespace (admin only).
func (h *Handlers) EnableProtection(w http.ResponseWriter, r *http.Request) {
	h.setProtection(w, r, true)
}

// DisableProtection removes the protect-deletion annotation on the vcluster namespace (admin only).
func (h *Handlers) DisableProtection(w http.ResponseWriter, r *http.Request) {
	h.setProtection(w, r, false)
}

// setProtection is the shared body of Enable/DisableProtection: it delegates
// to the service (which enforces admin RBAC) and maps the typed errors back to
// the HTMX toasts the UI expects.
func (h *Handlers) setProtection(w http.ResponseWriter, r *http.Request, enabled bool) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	st, err := h.svc.SetProtection(r.Context(), h.actor(r), name, env, enabled)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrForbidden):
			w.WriteHeader(http.StatusForbidden)
			h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
		case errors.Is(err, service.ErrK8sUnavailable):
			// env is forwarded to the service untouched (it resolves the default
			// itself); the default is only needed to name the environment in the
			// message.
			h.renderToast(w, "error", fmt.Sprintf("Client Kubernetes %s non disponible", reqEnv(r)))
		default:
			verb := "activation"
			logMsg := "EnableProtection failed"
			if !enabled {
				verb = "desactivation"
				logMsg = "DisableProtection failed"
			}
			slog.Error(logMsg, "env", env, "vcluster", name, "err", err)
			h.renderToast(w, "error", fmt.Sprintf("Erreur %s protection : %v", verb, err))
		}
		return
	}
	h.renderProtection(w, r, st)
}

// renderProtection renders the protection_status.html HTMX fragment from a
// service.ProtectionState.
func (h *Handlers) renderProtection(w http.ResponseWriter, r *http.Request, st service.ProtectionState) {
	// Detail dit POURQUOI la réponse est indisponible, et le fragment le montre.
	// Sans lui, le template — entièrement sous {{if .Enabled}} — rendait du vide :
	// l'admin voyait son « Chargement... » remplacé par rien, définitivement, le
	// conteneur n'ayant qu'un hx-trigger="load". C'est mieux que l'ancien
	// comportement, qui affichait un toggle « Inactive » sur une lecture ratée —
	// un mensonge —, mais l'absence se lit pareil pour qui regarde l'écran.
	// Trois états jusqu'au bout : protégé, pas protégé, pas lisible.
	h.renderPartial(w, "protection_status.html", map[string]interface{}{
		"Enabled":   st.Available,
		"Protected": st.Protected,
		"Detail":    st.Detail,
		"Name":      st.Name,
		"Env":       st.Env,
		"User":      h.getUser(r),
	})
}
