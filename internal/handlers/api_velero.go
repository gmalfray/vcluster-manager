package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
)

// backupNameRegex is the accepted shape of a Velero backup name: what Velero
// itself generates for a manual backup ("manual-<vcluster>-<millis>", see
// CreateVeleroBackup) and what a Schedule-driven one looks like
// ("<schedule>-<timestamp>") — lowercase alphanumerics, dots and dashes, i.e. a
// normal Kubernetes resource name. That rules out "/" and "..", which is what
// matters here: the name flows into a DownloadRequest object and an S3 URL,
// and gets fetched from an admin browser.
var backupNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

func validBackupName(backup string) bool {
	return backupNameRegex.MatchString(backup)
}

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

	if !validBackupName(backup) {
		h.renderPartial(w, "velero_backup_content.html", map[string]interface{}{
			"Error":      "Nom de backup invalide",
			"BackupName": backup,
		})
		return
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
	if !validBackupName(backupName) {
		h.renderToast(w, "error", "Nom de backup invalide")
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

	// An in-place restore overwrites the source vcluster: suspend Flux, scale it
	// down and delete its PVC so Velero can recreate it. Confirming the backup is
	// actually restorable comes first — finding out after the PVC is gone would
	// leave the vcluster on an empty volume.
	if inPlace {
		phase, err := k8s.GetVeleroBackupPhase(r.Context(), backupName, h.cfg.VeleroNamespace)
		if err != nil {
			h.renderToast(w, "error", fmt.Sprintf("Backup introuvable : %v", err))
			return
		}
		if phase != "Completed" {
			h.renderToast(w, "error", fmt.Sprintf("Backup non restaurable (phase : %s)", phase))
			return
		}

		if err := k8s.SetFluxSuspend(r.Context(), name, true); err != nil {
			slog.Warn("could not suspend flux", "vcluster", name, "err", err)
		}
		if err := k8s.ScaleVClusterStatefulSet(r.Context(), name, 0); err != nil {
			slog.Warn("could not scale down vcluster", "vcluster", name, "err", err)
		} else {
			// Wait for the pod to really terminate: deleting a still-mounted PVC
			// leaves it stuck Terminating.
			if err := k8s.WaitForVClusterPodGone(r.Context(), name, 30*time.Second); err != nil {
				slog.Warn("pod didn't terminate in time, deleting PVC anyway", "vcluster", name, "err", err)
			}
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

	// An in-place restore leaves Flux suspended and the StatefulSet at zero until
	// it ends. Browser polling can't be trusted with that — close the tab and the
	// vcluster stays down — so watch it server-side too.
	if inPlace {
		go h.resumeAfterInPlaceRestore(k8s, name, restoreName, h.cfg.VeleroNamespace)
	}

	h.renderPartial(w, "velero_restore_status.html", map[string]interface{}{
		"RestoreName": restoreName,
		"Phase":       "New",
		"Name":        name,
		"Env":         env,
		"BackupName":  backupName,
		"InPlace":     inPlace,
	})
}

// resumeAfterInPlaceRestore watches an in-place restore to completion and resumes
// Flux (which rescales the vcluster) independently of the browser. On timeout it
// resumes anyway rather than leave the vcluster stuck at zero replicas.
func (h *Handlers) resumeAfterInPlaceRestore(k8s *kubernetes.StatusClient, name, restoreName, veleroNamespace string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Warn("restore timed out, resuming flux as a safety net", "restore", restoreName, "vcluster", name)
			if err := k8s.SetFluxSuspend(context.Background(), name, false); err != nil {
				slog.Error("could not resume flux after timeout", "vcluster", name, "err", err)
			}
			return
		case <-ticker.C:
			phase, err := k8s.GetRestoreStatus(ctx, restoreName, veleroNamespace)
			if err != nil {
				slog.Warn("polling restore failed", "restore", restoreName, "err", err)
				continue
			}
			if isTerminalRestorePhase(phase) {
				if err := k8s.SetFluxSuspend(context.Background(), name, false); err != nil {
					slog.Error("could not resume flux after restore", "vcluster", name, "phase", phase, "err", err)
				} else {
					slog.Info("restore reached terminal phase, flux resumed", "restore", restoreName, "phase", phase, "vcluster", name)
				}
				return
			}
		}
	}
}

// isTerminalRestorePhase reports whether a restore is over, whatever the outcome.
func isTerminalRestorePhase(phase string) bool {
	return phase == "Completed" || phase == "Failed" || phase == "PartiallyFailed"
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
	if inPlace && isTerminalRestorePhase(phase) {
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
	if !validBackupName(backup) {
		h.renderToast(w, "error", "Nom de backup invalide")
		return
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
