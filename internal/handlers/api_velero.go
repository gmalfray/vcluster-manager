package handlers

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/audit"
)

// VeleroBackupList returns an HTMX fragment listing Velero backups for a vcluster.
func (h *Handlers) VeleroBackupList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderPartial(w, "velero_backups.html", map[string]interface{}{
			"Error": "Client Kubernetes non configuré",
			"Name":  name,
			"Env":   env,
		})
		return
	}

	backups, err := k8s.ListVeleroBackups(r.Context(), name, h.cfg.VeleroNamespace)
	if err != nil {
		h.renderPartial(w, "velero_backups.html", map[string]interface{}{
			"Error": err.Error(),
			"Name":  name,
			"Env":   env,
		})
		return
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].StartTime > backups[j].StartTime
	})

	// Also fetch active (non-terminal) restores so their polling survives page refresh
	activeRestores, _ := k8s.ListActiveVeleroRestores(r.Context(), name, h.cfg.VeleroNamespace)

	h.renderPartial(w, "velero_backups.html", map[string]interface{}{
		"Backups":        backups,
		"ActiveRestores": activeRestores,
		"Name":           name,
		"Env":            env,
		"User":           h.getUser(r),
	})
}

// VeleroBackupContent fetches the content URL for a backup and proxies the JSON result.
func (h *Handlers) VeleroBackupContent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	backup := r.PathValue("backup")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      "Client Kubernetes non configuré",
			"BackupName": backup,
		})
		return
	}

	ctx := r.Context()
	downloadURL, err := k8s.GetBackupContentURL(ctx, backup, h.cfg.VeleroNamespace)
	if err != nil {
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      fmt.Sprintf("Impossible d'obtenir le contenu : %v", err),
			"BackupName": backup,
		})
		return
	}

	// Fetch the JSON from the presigned URL
	resp, err := httpGetWithTimeout(downloadURL, 15*time.Second)
	if err != nil {
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      fmt.Sprintf("Téléchargement échoué : %v", err),
			"BackupName": backup,
		})
		return
	}
	defer resp.Body.Close()

	reader := io.Reader(resp.Body)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
				"Error":      fmt.Sprintf("Décompression échouée : %v", err),
				"BackupName": backup,
			})
			return
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}

	body, err := io.ReadAll(io.LimitReader(reader, 1<<20)) // 1MB max
	if err != nil {
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      fmt.Sprintf("Lecture du contenu échouée : %v", err),
			"BackupName": backup,
		})
		return
	}

	// Try gzip decompression even without Content-Encoding header (S3 may omit it)
	if len(body) > 1 && body[0] == 0x1f && body[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err == nil {
			defer func() { _ = gz.Close() }()
			if decompressed, err := io.ReadAll(io.LimitReader(gz, 1<<20)); err == nil {
				body = decompressed
			}
		}
	}

	// Pretty-print the JSON
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		pretty.Write(body)
	}

	h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
		"BackupName": backup,
		"Content":    pretty.String(),
		"Name":       name,
		"Env":        env,
	})
}

// CreateVeleroRestore initiates a Velero restore from a backup (admin only).
func (h *Handlers) CreateVeleroRestore(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}
	backupName := r.URL.Query().Get("backup")
	targetName := r.URL.Query().Get("target") // target vcluster name (empty = same vcluster)
	if backupName == "" {
		h.renderToast(w, "error", "Nom du backup manquant")
		return
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderToast(w, "error", "Client Kubernetes non configuré")
		return
	}

	sourceNS := "vcluster-" + name
	targetNS := "vcluster-" + name
	inPlace := targetName == "" || targetName == name
	if !inPlace {
		targetNS = "vcluster-" + targetName
	}

	// For in-place restores: suspend Flux, scale down vcluster, delete PVC so Velero can restore it.
	if inPlace {
		if err := k8s.SetFluxSuspend(r.Context(), name, true); err != nil {
			slog.Warn("could not suspend flux", "vcluster", name, "err", err)
		}
		if err := k8s.ScaleVClusterStatefulSet(r.Context(), name, 0); err != nil {
			slog.Warn("could not scale down vcluster", "vcluster", name, "err", err)
		} else {
			// Give the StatefulSet a moment to release the PVC before deleting it.
			time.Sleep(5 * time.Second)
			if err := k8s.DeleteVClusterPVC(r.Context(), name); err != nil {
				slog.Warn("could not delete PVC", "vcluster", name, "err", err)
			}
		}
	}

	restoreName, err := k8s.CreateVeleroRestore(r.Context(), backupName, sourceNS, targetNS, h.cfg.VeleroNamespace)
	if err != nil {
		// Resume Flux if restore creation failed (it will rescale the StatefulSet).
		if inPlace {
			if resumeErr := k8s.SetFluxSuspend(r.Context(), name, false); resumeErr != nil {
				slog.Warn("could not resume flux after failed restore", "vcluster", name, "err", resumeErr)
			}
		}
		h.renderToast(w, "error", fmt.Sprintf("Erreur création restore : %v", err))
		return
	}

	audit.Log(r, "velero-restore", name, env, "backup="+backupName, "target="+targetNS)
	h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
		"RestoreName": restoreName,
		"Phase":       "New",
		"Name":        name,
		"Env":         env,
		"BackupName":  backupName,
		"InPlace":     inPlace,
	})
}

// VeleroRestoreStatus returns the status of a Velero restore (HTMX polling).
func (h *Handlers) VeleroRestoreStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	restoreName := r.PathValue("restore")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}
	inPlace := r.URL.Query().Get("inplace") == "true"

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
			"Error":       "Client Kubernetes non configuré",
			"RestoreName": restoreName,
		})
		return
	}

	phase, err := k8s.GetRestoreStatus(r.Context(), restoreName, h.cfg.VeleroNamespace)
	if err != nil {
		h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
			"Error":       err.Error(),
			"RestoreName": restoreName,
			"Name":        name,
			"Env":         env,
			"InPlace":     inPlace,
		})
		return
	}

	// Resume Flux when restore is complete (in-place only).
	terminal := phase == "Completed" || phase == "Failed" || phase == "PartiallyFailed"
	if inPlace && terminal {
		if resumeErr := k8s.SetFluxSuspend(r.Context(), name, false); resumeErr != nil {
			slog.Warn("could not resume flux after restore", "vcluster", name, "err", resumeErr)
		}
	}

	h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
		"RestoreName": restoreName,
		"Phase":       phase,
		"Name":        name,
		"Env":         env,
		"InPlace":     inPlace,
	})
}

// TriggerVeleroBackup creates an on-demand Velero backup for a vcluster (admin only).
func (h *Handlers) TriggerVeleroBackup(w http.ResponseWriter, r *http.Request) {
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
		h.renderToast(w, "error", "Client Kubernetes non configuré")
		return
	}

	backupName, err := k8s.CreateVeleroBackup(r.Context(), name, h.cfg.VeleroNamespace, h.cfg.VeleroDefaultTTL, "")
	if err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur création backup : %v", err))
		return
	}

	audit.Log(r, "velero-backup-manual", name, env, "backup="+backupName)

	// Return a toast + trigger refresh of backup list
	w.Header().Set("HX-Trigger", `{"veleroBackupsRefresh": true}`)
	h.renderToast(w, "success", fmt.Sprintf("Backup déclenché : %s", backupName))
}

// DeleteVeleroBackup deletes a Velero backup (admin only, Failed/PartiallyFailed).
func (h *Handlers) DeleteVeleroBackup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	backup := r.PathValue("backup")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}

	k8s := h.k8sForEnv(env)
	if k8s == nil {
		h.renderToast(w, "error", "Client Kubernetes non configuré")
		return
	}

	if err := k8s.DeleteVeleroBackup(r.Context(), backup, h.cfg.VeleroNamespace); err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur suppression backup : %v", err))
		return
	}

	audit.Log(r, "velero-backup-delete", name, env, "backup="+backup)
	w.Header().Set("HX-Trigger", `{"veleroBackupsRefresh": true}`)
	h.renderToast(w, "success", fmt.Sprintf("Backup %s supprimé", backup))
}

// httpGetWithTimeout performs a GET request with a timeout.
func httpGetWithTimeout(url string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	return client.Get(url) //nolint:noctx
}
