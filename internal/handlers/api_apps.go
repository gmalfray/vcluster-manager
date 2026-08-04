package handlers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/service"
)

// ListApps returns an HTMX fragment listing ArgoCD Applications.
// It queries Application objects directly from the vcluster API (source of truth).
// Falls back to the app-manifests GitLab repo if the vcluster is unreachable.
func (h *Handlers) ListApps(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	targetVClusters := h.svc.MigrationTargets(r.Context(), name, env)

	// GetApps never actually returns an error today (see its doc comment) —
	// the return value is kept for symmetry with the REST adapter.
	apps, source, _ := h.svc.GetApps(r.Context(), name, env)

	// Migration state is tracked in memory by this adapter (getMigrationLabel),
	// not by the service — annotate the raw apps before rendering.
	for i := range apps {
		if label := h.getMigrationLabel(env, name, apps[i].Name); label != "" {
			apps[i].Migrating = true
			apps[i].MigratingLabel = label
		}
	}

	h.renderPartial(w, "apps_list.html", map[string]interface{}{
		"SourceVCluster":  name,
		"Env":             env,
		"User":            h.getUser(r),
		"TargetVClusters": targetVClusters,
		"Apps":            apps,
		"AppsSource":      source,
	})
}

// MigrateApp copies an ArgoCD Application from one vcluster's app-manifests to another (admin only).
func (h *Handlers) MigrateApp(w http.ResponseWriter, r *http.Request) {
	sourceName := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	appName := r.FormValue("app_name")
	filePath := r.FormValue("app_file_path")
	targetName := r.FormValue("target_vcluster")
	deleteSource := r.FormValue("delete_source") != ""

	result, err := h.svc.MigrateApp(r.Context(), h.actor(r), sourceName, env, appName, filePath, targetName, deleteSource)
	if err != nil {
		h.renderMigrateAppError(w, err, targetName)
		return
	}

	if result.DeleteSourceFailed {
		h.renderToast(w, "warning", fmt.Sprintf("App migree vers %s mais erreur suppression source : %s", targetName, result.DeleteSourceError))
		return
	}

	h.addMigration(env, sourceName, targetName, appName)
	h.renderToast(w, "success", fmt.Sprintf("App %s migree vers %s (%d fichiers)", appName, targetName, result.FilesMigrated))
}

// renderMigrateAppError maps a service.MigrateApp error to the exact toast
// (and status, for the forbidden case) the pre-extraction handler produced.
func (h *Handlers) renderMigrateAppError(w http.ResponseWriter, err error, targetName string) {
	switch {
	case errors.Is(err, service.ErrForbidden):
		w.WriteHeader(http.StatusForbidden)
		h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
	case errors.Is(err, service.ErrAppInvalidTarget):
		h.renderToast(w, "error", "Vcluster cible invalide")
	case errors.Is(err, service.ErrInvalidName):
		h.renderToast(w, "error", "Nom invalide : doit commencer par une lettre, uniquement [a-z0-9-]")
	case errors.Is(err, service.ErrAppMissingFilePath):
		h.renderToast(w, "error", "Chemin du fichier manquant")
	case errors.Is(err, service.ErrAppGitLabUnavailable):
		h.renderToast(w, "error", "GitLab client non disponible")
	default:
		var opErr *service.MigrateOpError
		if errors.As(err, &opErr) {
			switch opErr.Stage {
			case "list-source":
				h.renderToast(w, "error", fmt.Sprintf("Erreur liste fichiers source : %v", opErr.Err))
			case "read-file":
				h.renderToast(w, "error", fmt.Sprintf("Erreur lecture %s : %v", opErr.File, opErr.Err))
			case "commit-target":
				h.renderToast(w, "error", fmt.Sprintf("Erreur migration vers %s : %v", targetName, opErr.Err))
			default:
				h.renderToast(w, "error", fmt.Sprintf("Erreur migration : %v", opErr.Err))
			}
			return
		}
		h.renderToast(w, "error", fmt.Sprintf("Erreur migration : %v", err))
	}
}
