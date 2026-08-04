package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// backupNameRegex is the accepted shape of a Velero backup name: what Velero
// itself generates for a manual backup ("manual-<vcluster>-<millis>", see
// TriggerVeleroBackup) and what a Schedule-driven one looks like
// ("<schedule>-<timestamp>") — lowercase alphanumerics, dots and dashes, i.e. a
// normal Kubernetes resource name. That rules out "/" and "..", which is what
// matters here: the name flows into a DownloadRequest object and an S3 URL,
// and gets fetched from an admin browser.
var backupNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// validBackupName reports whether backup matches backupNameRegex.
func validBackupName(backup string) bool {
	return backupNameRegex.MatchString(backup)
}

// Sentinel errors for the Velero domain, on top of ErrForbidden and
// ErrK8sUnavailable declared in service.go.
var (
	ErrBackupNameRequired       = errors.New("backup name required")
	ErrInvalidBackupName        = errors.New("invalid backup name")
	ErrBackupLookupFailed       = errors.New("backup lookup failed")
	ErrBackupContentUnavailable = errors.New("backup content unavailable")
	ErrBackupDownloadFailed     = errors.New("backup content download failed")
	ErrBackupDecompressFailed   = errors.New("backup content decompression failed")
	ErrBackupReadFailed         = errors.New("backup content read failed")
)

// ErrBackupNotRestorable means an in-place restore was requested from a backup
// that hasn't reached the Completed phase. Checked BEFORE the destructive part
// of an in-place restore (scaling the vcluster down and deleting its PVC) —
// finding out the backup is unusable after the PVC is already gone would leave
// the vcluster on an empty volume.
type ErrBackupNotRestorable struct {
	Phase string
}

func (e *ErrBackupNotRestorable) Error() string {
	return fmt.Sprintf("backup not restorable (phase: %s)", e.Phase)
}

// stageErr pairs a domain sentinel with the underlying cause so an adapter can
// both errors.Is() against the sentinel (to pick the right toast) and get the
// original error text back via Error() — the k8s/HTTP message reaches the UI
// unchanged, only prefixed by the adapter.
type stageErr struct {
	sentinel error
	cause    error
}

func (e *stageErr) Error() string        { return e.cause.Error() }
func (e *stageErr) Unwrap() error        { return e.cause }
func (e *stageErr) Is(target error) bool { return target == e.sentinel }

// isTerminalRestorePhase reports whether a restore is over, whatever the outcome.
func isTerminalRestorePhase(phase string) bool {
	return phase == "Completed" || phase == "Failed" || phase == "PartiallyFailed"
}

// VeleroBackupsView lists the Velero backups for a vcluster, plus any restore
// still in progress (so its polling survives a page refresh).
type VeleroBackupsView struct {
	Backups        []models.VeleroBackupInfo
	ActiveRestores []models.VeleroRestoreInfo
	Name           string
	Env            string
}

// GetVeleroBackups lists Velero backups targeting a vcluster, newest first.
// Read-only, no privilege required.
func (s *Service) GetVeleroBackups(ctx context.Context, name, env string) (VeleroBackupsView, error) {
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return VeleroBackupsView{}, ErrK8sUnavailable
	}

	backups, err := k8s.ListVeleroBackups(ctx, name, s.cfg.VeleroNamespace)
	if err != nil {
		return VeleroBackupsView{}, err
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].StartTime > backups[j].StartTime
	})

	// Active restores are best-effort: their absence shouldn't hide the backup list.
	activeRestores, _ := k8s.ListActiveVeleroRestores(ctx, name, s.cfg.VeleroNamespace)

	return VeleroBackupsView{
		Backups:        backups,
		ActiveRestores: activeRestores,
		Name:           name,
		Env:            env,
	}, nil
}

// VeleroBackupContentView is the pretty-printed JSON content of a backup's
// resource list.
type VeleroBackupContentView struct {
	BackupName string
	Content    string
	Name       string
	Env        string
}

// GetVeleroBackupContent fetches and pretty-prints the resource list of a
// backup. Admin only: the content is the tenant's raw resource dump, secrets
// included, unlike GetVeleroBackups which only lists metadata.
func (s *Service) GetVeleroBackupContent(ctx context.Context, actor models.Actor, name, backup, env string) (VeleroBackupContentView, error) {
	if !actor.IsAdmin {
		return VeleroBackupContentView{}, ErrForbidden
	}
	if !validBackupName(backup) {
		return VeleroBackupContentView{}, ErrInvalidBackupName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return VeleroBackupContentView{}, ErrK8sUnavailable
	}

	downloadURL, err := k8s.GetBackupContentURL(ctx, backup, s.cfg.VeleroNamespace)
	if err != nil {
		return VeleroBackupContentView{}, &stageErr{ErrBackupContentUnavailable, err}
	}

	resp, err := httpGetWithTimeout(downloadURL, 15*time.Second)
	if err != nil {
		return VeleroBackupContentView{}, &stageErr{ErrBackupDownloadFailed, err}
	}
	defer resp.Body.Close()

	reader := io.Reader(resp.Body)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return VeleroBackupContentView{}, &stageErr{ErrBackupDecompressFailed, err}
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}

	body, err := io.ReadAll(io.LimitReader(reader, 1<<20)) // 1MB max
	if err != nil {
		return VeleroBackupContentView{}, &stageErr{ErrBackupReadFailed, err}
	}

	// Try gzip decompression even without a Content-Encoding header (S3 may omit it).
	if len(body) > 1 && body[0] == 0x1f && body[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err == nil {
			defer func() { _ = gz.Close() }()
			if decompressed, err := io.ReadAll(io.LimitReader(gz, 1<<20)); err == nil {
				body = decompressed
			}
		}
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		pretty.Write(body)
	}

	return VeleroBackupContentView{
		BackupName: backup,
		Content:    pretty.String(),
		Name:       name,
		Env:        env,
	}, nil
}

// httpGetWithTimeout performs a GET request with a timeout, used to fetch a
// Velero backup's presigned content URL.
func httpGetWithTimeout(url string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	return client.Get(url) //nolint:noctx
}

// VeleroRestoreView is the state of a just-created Velero restore.
type VeleroRestoreView struct {
	RestoreName string
	Phase       string
	Name        string
	Env         string
	BackupName  string
	InPlace     bool
}

// CreateVeleroRestore starts a Velero restore of backupName into targetName
// (empty or equal to name means an in-place restore of the same vcluster).
// Admin only.
//
// An in-place restore overwrites the source vcluster's volume, so before
// touching anything it confirms the backup is actually restorable (phase
// Completed). Only then does it suspend Flux, scale the vcluster down, wait
// for its pod to really terminate and delete its PVC so Velero can recreate
// it from the backup.
func (s *Service) CreateVeleroRestore(ctx context.Context, actor models.Actor, name, env, backupName, targetName string) (VeleroRestoreView, error) {
	if !actor.IsAdmin {
		return VeleroRestoreView{}, ErrForbidden
	}
	if backupName == "" {
		return VeleroRestoreView{}, ErrBackupNameRequired
	}
	if !validBackupName(backupName) {
		return VeleroRestoreView{}, ErrInvalidBackupName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return VeleroRestoreView{}, ErrK8sUnavailable
	}

	sourceNS := "vcluster-" + name
	targetNS := "vcluster-" + name
	inPlace := targetName == "" || targetName == name
	if !inPlace {
		targetNS = "vcluster-" + targetName
	}

	if inPlace {
		phase, err := k8s.GetVeleroBackupPhase(ctx, backupName, s.cfg.VeleroNamespace)
		if err != nil {
			return VeleroRestoreView{}, &stageErr{ErrBackupLookupFailed, err}
		}
		if phase != "Completed" {
			return VeleroRestoreView{}, &ErrBackupNotRestorable{Phase: phase}
		}

		if err := k8s.SetFluxSuspend(ctx, name, true); err != nil {
			slog.Warn("could not suspend flux", "vcluster", name, "err", err)
		}
		if err := k8s.ScaleVClusterStatefulSet(ctx, name, 0); err != nil {
			slog.Warn("could not scale down vcluster", "vcluster", name, "err", err)
		} else {
			// Wait for the pod to really terminate: deleting a still-mounted PVC
			// leaves it stuck Terminating.
			if err := k8s.WaitForVClusterPodGone(ctx, name, 30*time.Second); err != nil {
				slog.Warn("pod didn't terminate in time, deleting PVC anyway", "vcluster", name, "err", err)
			}
			if err := k8s.DeleteVClusterPVC(ctx, name); err != nil {
				slog.Warn("could not delete PVC", "vcluster", name, "err", err)
			}
		}
	}

	restoreName, err := k8s.CreateVeleroRestore(ctx, backupName, sourceNS, targetNS, s.cfg.VeleroNamespace)
	if err != nil {
		// Resume Flux if restore creation failed (it will rescale the StatefulSet).
		if inPlace {
			if resumeErr := k8s.SetFluxSuspend(ctx, name, false); resumeErr != nil {
				slog.Warn("could not resume flux after failed restore", "vcluster", name, "err", resumeErr)
			}
		}
		return VeleroRestoreView{}, err
	}

	audit.LogActor(actor.Username, "velero-restore", name, env, "backup="+backupName, "target="+targetNS)

	// An in-place restore leaves Flux suspended and the StatefulSet at zero until
	// it ends. Browser polling can't be trusted with that — close the tab and the
	// vcluster stays down — so watch it here too, independent of the request.
	if inPlace {
		go s.resumeAfterInPlaceRestore(k8s, name, restoreName, s.cfg.VeleroNamespace)
	}

	return VeleroRestoreView{
		RestoreName: restoreName,
		Phase:       "New",
		Name:        name,
		Env:         env,
		BackupName:  backupName,
		InPlace:     inPlace,
	}, nil
}

// resumeAfterInPlaceRestore watches an in-place restore to completion and
// resumes Flux (which rescales the vcluster) independently of the request
// that started it. On timeout it resumes anyway rather than leave the
// vcluster stuck at zero replicas.
func (s *Service) resumeAfterInPlaceRestore(k8s *kubernetes.StatusClient, name, restoreName, veleroNamespace string) {
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

// VeleroRestoreStatusView is a polled Velero restore's current phase.
type VeleroRestoreStatusView struct {
	RestoreName string
	Phase       string
	Name        string
	Env         string
	InPlace     bool
}

// GetVeleroRestoreStatus polls the phase of a Velero restore. When inPlace and
// the restore has reached a terminal phase, it resumes Flux (which rescales
// the vcluster) — a second, request-driven path to the same effect as
// resumeAfterInPlaceRestore's background poll. Whichever notices completion
// first wins; resuming twice is harmless (SetFluxSuspend is idempotent).
func (s *Service) GetVeleroRestoreStatus(ctx context.Context, name, restoreName, env string, inPlace bool) (VeleroRestoreStatusView, error) {
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return VeleroRestoreStatusView{}, ErrK8sUnavailable
	}

	phase, err := k8s.GetRestoreStatus(ctx, restoreName, s.cfg.VeleroNamespace)
	if err != nil {
		return VeleroRestoreStatusView{}, err
	}

	if inPlace && isTerminalRestorePhase(phase) {
		if resumeErr := k8s.SetFluxSuspend(ctx, name, false); resumeErr != nil {
			slog.Warn("could not resume flux after restore", "vcluster", name, "err", resumeErr)
		}
	}

	return VeleroRestoreStatusView{
		RestoreName: restoreName,
		Phase:       phase,
		Name:        name,
		Env:         env,
		InPlace:     inPlace,
	}, nil
}

// VeleroBackupCreated is the result of triggering an on-demand backup.
type VeleroBackupCreated struct {
	BackupName string
	Name       string
	Env        string
}

// TriggerVeleroBackup creates an on-demand Velero backup for a vcluster.
// Admin only.
func (s *Service) TriggerVeleroBackup(ctx context.Context, actor models.Actor, name, env string) (VeleroBackupCreated, error) {
	if !actor.IsAdmin {
		return VeleroBackupCreated{}, ErrForbidden
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return VeleroBackupCreated{}, ErrK8sUnavailable
	}

	backupName, err := k8s.CreateVeleroBackup(ctx, name, s.cfg.VeleroNamespace, s.cfg.VeleroDefaultTTL, "")
	if err != nil {
		return VeleroBackupCreated{}, err
	}

	audit.LogActor(actor.Username, "velero-backup-manual", name, env, "backup="+backupName)

	return VeleroBackupCreated{BackupName: backupName, Name: name, Env: env}, nil
}

// DeleteVeleroBackup deletes a Velero backup object. Admin only.
func (s *Service) DeleteVeleroBackup(ctx context.Context, actor models.Actor, name, backup, env string) (string, error) {
	if !actor.IsAdmin {
		return "", ErrForbidden
	}
	if !validBackupName(backup) {
		return "", ErrInvalidBackupName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return "", ErrK8sUnavailable
	}

	if err := k8s.DeleteVeleroBackup(ctx, backup, s.cfg.VeleroNamespace); err != nil {
		return "", err
	}

	audit.LogActor(actor.Username, "velero-backup-delete", name, env, "backup="+backup)

	return backup, nil
}
