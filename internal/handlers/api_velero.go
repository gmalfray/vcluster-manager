package handlers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/service"
)

// VeleroBackupList returns an HTMX fragment listing Velero backups for a vcluster.
func (h *Handlers) VeleroBackupList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := reqEnv(r)

	view, err := h.svc.GetVeleroBackups(r.Context(), name, env)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, service.ErrK8sUnavailable) {
			msg = "Client Kubernetes non configuré"
		}
		h.renderPartial(w, "velero_backups.html", map[string]interface{}{
			"Error": msg,
			"Name":  name,
			"Env":   env,
		})
		return
	}

	h.renderPartial(w, "velero_backups.html", map[string]interface{}{
		"Backups":        view.Backups,
		"ActiveRestores": view.ActiveRestores,
		"Name":           view.Name,
		"Env":            view.Env,
		"User":           h.getUser(r),
	})
}

// VeleroBackupContent fetches the content URL for a backup and proxies the JSON result.
func (h *Handlers) VeleroBackupContent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	backup := r.PathValue("backup")
	env := reqEnv(r)

	view, err := h.svc.GetVeleroBackupContent(r.Context(), h.actor(r), name, backup, env)
	if err != nil {
		if errors.Is(err, service.ErrForbidden) {
			w.WriteHeader(http.StatusForbidden)
			h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
			return
		}
		var msg string
		switch {
		case errors.Is(err, service.ErrInvalidBackupName):
			msg = "Nom de backup invalide"
		case errors.Is(err, service.ErrK8sUnavailable):
			msg = "Client Kubernetes non configuré"
		case errors.Is(err, service.ErrBackupContentUnavailable):
			msg = fmt.Sprintf("Impossible d'obtenir le contenu : %v", err)
		case errors.Is(err, service.ErrBackupDownloadFailed):
			msg = fmt.Sprintf("Téléchargement échoué : %v", err)
		case errors.Is(err, service.ErrBackupDecompressFailed):
			msg = fmt.Sprintf("Décompression échouée : %v", err)
		case errors.Is(err, service.ErrBackupReadFailed):
			msg = fmt.Sprintf("Lecture du contenu échouée : %v", err)
		default:
			msg = err.Error()
		}
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      msg,
			"BackupName": backup,
		})
		return
	}

	h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
		"BackupName": view.BackupName,
		"Content":    view.Content,
		"Name":       view.Name,
		"Env":        view.Env,
	})
}

// restorePollURL is where the restore status partial polls itself. Two shapes,
// because the two paths track different objects: on the direct path a Velero
// Restore exists and has a name; on the deferred path the operator has not
// created it yet, so the marker's status is the only thing to follow. Computing
// it here keeps that difference out of the template.
func restorePollURL(name, restoreName, env string, inPlace bool) string {
	if restoreName == "" {
		return fmt.Sprintf("/api/vclusters/%s/velero/ops/restore/status?env=%s", name, env)
	}
	suffix := ""
	if inPlace {
		suffix = "&inplace=true"
	}
	return fmt.Sprintf("/api/vclusters/%s/velero/restore/%s/status?env=%s%s", name, restoreName, env, suffix)
}

// CreateVeleroRestore initiates a Velero restore from a backup (admin only, enforced by the service).
func (h *Handlers) CreateVeleroRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	backupName := r.URL.Query().Get("backup")
	targetName := r.URL.Query().Get("target") // target vcluster name (empty = same vcluster)

	// StartVeleroRestore, not CreateVeleroRestore: the service decides whether the
	// sequence runs here or is handed to the operator (VELERO_TRIGGER_MODE).
	view, err := h.svc.StartVeleroRestore(r.Context(), h.actor(r), name, env, backupName, targetName)
	if err != nil {
		var notRestorable *service.ErrBackupNotRestorable
		switch {
		case errors.Is(err, service.ErrForbidden):
			w.WriteHeader(http.StatusForbidden)
			h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
		case errors.Is(err, service.ErrBackupNameRequired):
			h.renderToast(w, "error", "Nom du backup manquant")
		case errors.Is(err, service.ErrInvalidBackupName):
			h.renderToast(w, "error", "Nom de backup invalide")
		case errors.Is(err, service.ErrInvalidName):
			h.renderToast(w, "error", "Nom invalide : doit commencer par une lettre, uniquement [a-z0-9-]")
		case errors.Is(err, service.ErrK8sUnavailable):
			h.renderToast(w, "error", "Client Kubernetes non configuré")
		case errors.Is(err, service.ErrBackupLookupFailed):
			h.renderToast(w, "error", fmt.Sprintf("Backup introuvable : %v", err))
		case errors.As(err, &notRestorable):
			h.renderToast(w, "error", fmt.Sprintf("Backup non restaurable (phase : %s)", notRestorable.Phase))
		case errors.Is(err, service.ErrRestoreStageFailedVolumeGone):
			w.WriteHeader(http.StatusInternalServerError)
			h.renderToast(w, "error", fmt.Sprintf("Volume déjà supprimé, restore non créé : relancez un restore (le flux reste suspendu) — %v", err))
		case errors.Is(err, service.ErrRestoreStageFailed):
			w.WriteHeader(http.StatusInternalServerError)
			h.renderToast(w, "error", fmt.Sprintf("Restauration interrompue : %v", err))
		default:
			h.renderToast(w, "error", fmt.Sprintf("Erreur création restore : %v", err))
		}
		return
	}

	h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
		"RestoreName":     view.RestoreName,
		"Phase":           view.Phase,
		"Name":            view.Name,
		"Env":             view.Env,
		"BackupName":      view.BackupName,
		"InPlace":         view.InPlace,
		"ResumePending":   false,
		"ResumeFailed":    false,
		"VolumeDestroyed": false,
		"PollURL":         restorePollURL(view.Name, view.RestoreName, view.Env, view.InPlace),
	})
}

// VeleroOpsRestoreStatus polls the restore status the operator publishes on the
// marker (HTMX). Used on the deferred path, where there is no Velero Restore name
// to poll until the operator has created one.
func (h *Handlers) VeleroOpsRestoreStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := reqEnv(r)

	view, err := h.svc.GetVeleroOpsRestoreStatus(r.Context(), name, env)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, service.ErrK8sUnavailable) {
			msg = "Client Kubernetes non configuré"
		}
		h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
			"Error": msg,
			"Name":  name,
			"Env":   env,
		})
		return
	}

	h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
		"RestoreName":     view.RestoreName,
		"Phase":           view.Phase,
		"Name":            view.Name,
		"Env":             view.Env,
		"InPlace":         view.InPlace,
		"ResumePending":   view.ResumePending,
		"ResumeFailed":    view.ResumeFailed,
		"ResumeError":     view.ResumeError,
		"VolumeDestroyed": view.VolumeDestroyed,
		// Keep polling the marker even once a restore name appears: the operator
		// stays the single writer of this status, and its resume outcome is
		// published there.
		"PollURL": restorePollURL(view.Name, "", view.Env, view.InPlace),
	})
}

// VeleroRestoreStatus returns the status of a Velero restore (HTMX polling).
func (h *Handlers) VeleroRestoreStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	restoreName := r.PathValue("restore")
	env := reqEnv(r)
	inPlace := r.URL.Query().Get("inplace") == "true"

	view, err := h.svc.GetVeleroRestoreStatus(r.Context(), name, restoreName, env, inPlace)
	if err != nil {
		if errors.Is(err, service.ErrK8sUnavailable) {
			h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
				"Error":       "Client Kubernetes non configuré",
				"RestoreName": restoreName,
			})
			return
		}
		h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
			"Error":       err.Error(),
			"RestoreName": restoreName,
			"Name":        name,
			"Env":         env,
			"InPlace":     inPlace,
		})
		return
	}

	h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
		"RestoreName":     view.RestoreName,
		"Phase":           view.Phase,
		"Name":            view.Name,
		"Env":             view.Env,
		"InPlace":         view.InPlace,
		"ResumePending":   view.ResumePending,
		"ResumeFailed":    view.ResumeFailed,
		"ResumeError":     view.ResumeError,
		"VolumeDestroyed": view.VolumeDestroyed,
		"PollURL":         restorePollURL(view.Name, view.RestoreName, view.Env, view.InPlace),
	})
}

// TriggerVeleroBackup creates an on-demand Velero backup for a vcluster (admin only, enforced by the service).
func (h *Handlers) TriggerVeleroBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")

	// StartVeleroBackup, not TriggerVeleroBackup: the service decides whether the
	// backup is created here or handed to the operator (VELERO_TRIGGER_MODE), so
	// this handler does not have to know which.
	res, err := h.svc.StartVeleroBackup(r.Context(), h.actor(r), name, env)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrForbidden):
			w.WriteHeader(http.StatusForbidden)
			h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
		case errors.Is(err, service.ErrK8sUnavailable):
			h.renderToast(w, "error", "Client Kubernetes non configuré")
		default:
			h.renderToast(w, "error", fmt.Sprintf("Erreur création backup : %v", err))
		}
		return
	}

	// Toast + trigger refresh of the backup list.
	w.Header().Set("HX-Trigger", `{"veleroBackupsRefresh": true}`)
	if res.Deferred() {
		// The backup does not exist yet — saying "backup déclenché : <nom>" would
		// name something that hasn't been created. The list refresh will show it
		// as soon as the operator has done the work.
		h.renderToast(w, "success", "Backup demandé, l'opérateur s'en charge")
		return
	}
	h.renderToast(w, "success", fmt.Sprintf("Backup déclenché : %s", res.BackupName))
}

// DeleteVeleroBackup deletes a Velero backup (admin only, enforced by the service).
func (h *Handlers) DeleteVeleroBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	backup := r.PathValue("backup")
	env := r.URL.Query().Get("env")

	deleted, err := h.svc.DeleteVeleroBackup(r.Context(), h.actor(r), name, backup, env)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrForbidden):
			w.WriteHeader(http.StatusForbidden)
			h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
		case errors.Is(err, service.ErrInvalidBackupName):
			h.renderToast(w, "error", "Nom de backup invalide")
		case errors.Is(err, service.ErrK8sUnavailable):
			h.renderToast(w, "error", "Client Kubernetes non configuré")
		default:
			h.renderToast(w, "error", fmt.Sprintf("Erreur suppression backup : %v", err))
		}
		return
	}

	w.Header().Set("HX-Trigger", `{"veleroBackupsRefresh": true}`)
	h.renderToast(w, "success", fmt.Sprintf("Backup %s supprimé", deleted))
}
