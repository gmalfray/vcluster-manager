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

// ErrRestoreStageFailed means one of the steps of an in-place restore
// (suspending Flux, scaling the vcluster down, deleting its PVC, creating the
// Restore object) failed. Unlike a transient hiccup we can shrug off, each of
// these steps has to actually happen for the restore to do anything — so on
// failure CreateVeleroRestore stops right there instead of pressing on with a
// restore that can't work, and reports it as an error rather than a stage it
// merely logged. See stageErr for what errors.Is(err, ErrRestoreStageFailed)
// unwraps to.
var ErrRestoreStageFailed = errors.New("restore stage failed")

// ErrRestoreStageFailedVolumeGone means the Restore object couldn't be
// created after the vcluster's PVC was already deleted. The backup itself is
// fine — nothing was lost — but Flux must NOT be resumed on this path: that
// would let the StatefulSet recreate an empty PVC, which hides the failure
// and gets in the way of a retry (Velero skips resources that already
// exist). The vcluster is left suspended and at zero replicas; what actually
// fixes it is trying the restore again.
var ErrRestoreStageFailedVolumeGone = errors.New("restore stage failed after volume deletion")

// CreateVeleroRestore starts a Velero restore of backupName into targetName
// (empty or equal to name means an in-place restore of the same vcluster).
// Admin only.
//
// An in-place restore overwrites the source vcluster's volume, so before
// touching anything it confirms the backup is actually restorable (phase
// Completed). Only then does it suspend Flux, scale the vcluster down, wait
// for its pods to really terminate and delete its PVC so Velero can recreate
// it from the backup. If a step before the PVC deletion fails, it aborts and
// best-effort resumes Flux rather than pressing on toward a restore that
// can't work — see ErrRestoreStageFailed. Once the PVC is gone, though,
// resuming Flux would just let it come back empty, so a failure past that
// point (creating the Restore object) leaves the vcluster suspended instead
// — see ErrRestoreStageFailedVolumeGone.
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
	if targetName != "" && !validName(targetName) {
		return VeleroRestoreView{}, ErrInvalidName
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

	// pvcDeleted marks the point of no return: once the PVC is gone, aborting
	// by resuming Flux is the wrong move (see ErrRestoreStageFailedVolumeGone).
	// Before that, it's still safe — the volume is intact.
	var pvcDeleted bool
	if inPlace {
		phase, err := k8s.GetVeleroBackupPhase(ctx, backupName, s.cfg.VeleroNamespace)
		if err != nil {
			return VeleroRestoreView{}, &stageErr{ErrBackupLookupFailed, err}
		}
		if phase != "Completed" {
			return VeleroRestoreView{}, &ErrBackupNotRestorable{Phase: phase}
		}

		if err := k8s.SetFluxSuspend(ctx, name, true); err != nil {
			return VeleroRestoreView{}, &stageErr{ErrRestoreStageFailed, fmt.Errorf("suspension de Flux : %w", err)}
		}
		if err := k8s.ScaleVClusterWorkloads(ctx, name, 0); err != nil {
			s.abortInPlaceRestore(k8s, name)
			return VeleroRestoreView{}, &stageErr{ErrRestoreStageFailed, fmt.Errorf("mise à l'échelle à 0 : %w", err)}
		}
		// Wait for the pods to really terminate: deleting a still-mounted PVC
		// leaves it stuck Terminating. A timeout here isn't fatal on its own —
		// we still attempt the delete, which is where a genuinely stuck pod
		// would surface as an error.
		if err := k8s.WaitForVClusterPodsGone(ctx, name, 30*time.Second); err != nil {
			slog.Warn("pods didn't terminate in time, deleting PVC anyway", "vcluster", name, "err", err)
		}
		if err := k8s.DeleteVClusterPVC(ctx, name); err != nil {
			s.abortInPlaceRestore(k8s, name)
			return VeleroRestoreView{}, &stageErr{ErrRestoreStageFailed, fmt.Errorf("suppression du volume : %w", err)}
		}
		pvcDeleted = true
	}

	restoreName, err := k8s.CreateVeleroRestore(ctx, backupName, sourceNS, targetNS, s.cfg.VeleroNamespace)
	if err != nil {
		if inPlace {
			if pvcDeleted {
				// The volume is already gone. The backup itself is still fine,
				// but resuming Flux here would let the StatefulSet recreate an
				// empty PVC and mask what actually happened — leave it
				// suspended so a retry starts from a clean slate.
				return VeleroRestoreView{}, &stageErr{ErrRestoreStageFailedVolumeGone, fmt.Errorf("création du restore : %w (volume déjà supprimé, un nouveau restore est nécessaire)", err)}
			}
			s.abortInPlaceRestore(k8s, name)
		}
		return VeleroRestoreView{}, &stageErr{ErrRestoreStageFailed, fmt.Errorf("création du restore : %w", err)}
	}

	audit.LogActor(actor.Username, "velero-restore", name, env, "backup="+backupName, "target="+targetNS)

	// An in-place restore leaves Flux suspended and the vcluster at zero
	// replicas until it ends. Browser polling can't be trusted with that —
	// close the tab and the vcluster stays down — so watch it here too,
	// independent of the request.
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

// abortInPlaceRestore resumes Flux after a stage of the in-place restore
// sequence failed partway through, before the PVC was deleted — so the
// vcluster isn't left stuck suspended and scaled to zero over a volume
// that's still intact. Once the PVC is gone, CreateVeleroRestore stops
// calling this — see ErrRestoreStageFailedVolumeGone. Best-effort: if this
// also fails, an operator now has two things to fix, but at least they'll
// know about both — the caller's error already carries the primary failure.
func (s *Service) abortInPlaceRestore(k8s *kubernetes.StatusClient, name string) {
	if err := k8s.SetFluxSuspend(context.Background(), name, false); err != nil {
		slog.Error("could not resume flux after aborting in-place restore", "vcluster", name, "err", err)
	}
}

// veleroResumeState is the settled outcome of resuming Flux after an
// in-place restore reaches a terminal phase. Both resumeAfterInPlaceRestore
// (the background watcher) and GetVeleroRestoreStatus (a request-driven
// poll) can attempt the resume, and either can be the one that resolves it —
// but a single failed attempt from either side isn't the final word: the
// background watcher keeps retrying every 10s, so nothing is reported to the
// UI as failed until it's actually given up.
type veleroResumeState struct {
	failed bool
	errMsg string
}

// veleroResumeResult reports what's known about restoreName's Flux resume,
// if it has been settled yet.
func (s *Service) veleroResumeResult(restoreName string) (state veleroResumeState, resolved bool) {
	s.veleroResumeMu.Lock()
	defer s.veleroResumeMu.Unlock()
	st, ok := s.veleroResumeStates[restoreName]
	if !ok {
		return veleroResumeState{}, false
	}
	return *st, true
}

// resolveVeleroResume records restoreName's Flux resume as settled, for
// whichever of the background watcher or a request-driven poll gets there
// first.
func (s *Service) resolveVeleroResume(restoreName string, failed bool, errMsg string) {
	s.veleroResumeMu.Lock()
	defer s.veleroResumeMu.Unlock()
	if s.veleroResumeStates == nil {
		s.veleroResumeStates = map[string]*veleroResumeState{}
	}
	s.veleroResumeStates[restoreName] = &veleroResumeState{failed: failed, errMsg: errMsg}
}

// resumeAfterInPlaceRestore watches an in-place restore to completion and
// resumes Flux (which rescales the vcluster) independently of the request
// that started it — a closed browser tab must not leave the vcluster stuck
// suspended. Once the restore reaches a terminal phase it keeps retrying the
// resume itself, every 10s, until it succeeds or the 2h deadline below gives
// up on its behalf. Either way the outcome is recorded via
// resolveVeleroResume so GetVeleroRestoreStatus can report it instead of
// guessing from a single attempt of its own.
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
//
// ResumeFailed and ResumeError are the data-safety contract with the
// adapters: once Phase is terminal for an in-place restore, the UI must not
// say "Flux repris" (resumed) unless ResumeFailed is false. A failed resume
// leaves the vcluster suspended and at zero replicas — a real problem the
// operator needs to know about, not something to paper over with a success
// message.
type VeleroRestoreStatusView struct {
	RestoreName  string
	Phase        string
	Name         string
	Env          string
	InPlace      bool
	ResumeFailed bool
	ResumeError  string
}

// GetVeleroRestoreStatus polls the phase of a Velero restore. When inPlace and
// the restore has reached a terminal phase, it resumes Flux (which rescales
// the vcluster) — a second, request-driven path to the same effect as
// resumeAfterInPlaceRestore's background poll. Whichever notices completion
// first wins; resuming twice is harmless (SetFluxSuspend is idempotent). If
// the resume itself fails, that's reported via ResumeFailed/ResumeError
// rather than swallowed — the restore may well have succeeded while Flux
// stayed stuck suspended, and the UI must not claim otherwise.
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

	var resumeFailed bool
	var resumeErrMsg string
	if inPlace && isTerminalRestorePhase(phase) {
		if resumeErr := k8s.SetFluxSuspend(ctx, name, false); resumeErr != nil {
			slog.Warn("could not resume flux after restore", "vcluster", name, "err", resumeErr)
			resumeFailed = true
			resumeErrMsg = resumeErr.Error()
		}
	}

	return VeleroRestoreStatusView{
		RestoreName:  restoreName,
		Phase:        phase,
		Name:         name,
		Env:          env,
		InPlace:      inPlace,
		ResumeFailed: resumeFailed,
		ResumeError:  resumeErrMsg,
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
