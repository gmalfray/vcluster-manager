package handlers

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/audit"
)

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
