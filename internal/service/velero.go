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

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
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

// ErrCrossVClusterRestoreUnsupported means an admin asked to restore a
// backup taken from one vcluster INTO another vcluster's data.
//
// D4 (docs/recette-restauration.md): refused rather than attempted, because
// what CreateVeleroRestore can actually ask Velero to do — a Restore with
// namespaceMapping — does not do what the feature's own label promises.
// Velero runs on the HOST cluster and has no notion of "inside vcluster B's
// own etcd"; namespaceMapping only relocates HOST-level objects from one
// namespace name to another. So instead of migrating the source's data into
// the target, it deposits a SECOND, full clone of the source's control-plane
// (Deployment/StatefulSet, PVC, Secrets, HelmRelease, a second ResourceQuota,
// Flux Kustomizations) into the target's namespace. Confirmed on the recette
// cell: 24 objects cloned into the target, its control-plane in
// CrashLoopBackOff, two ResourceQuota fighting over one budget — and cleaning
// up the clone's HelmRelease triggered a Helm uninstall that wiped the
// SOURCE vcluster's own workloads in the process.
//
// A restore that actually migrates data between vclusters needs a different
// mechanism: restore the source into a scratch namespace, bring it up on its
// own, then move its data into the target namespace by namespace — a feature
// to design and build, not a fix to this Restore spec.
var ErrCrossVClusterRestoreUnsupported = errors.New("cross-vcluster restore is not supported: it clones the source's control-plane into the target's namespace instead of migrating its data (see docs/recette-restauration.md D4)")

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

// IsTerminalRestorePhase reports whether a restore is over, whatever the
// outcome. Exported because an operator-style caller needs the same notion of
// "settled" as the service does, and two copies of it would drift.
func IsTerminalRestorePhase(phase string) bool {
	return phase == "Completed" || phase == "Failed" || phase == "PartiallyFailed"
}

// errText returns err's message, or "" for a nil err — for recording a
// resolved outcome that needs a string either way.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
	return s.createVeleroRestore(ctx, actor, name, env, backupName, targetName, false)
}

// CreateVeleroRestoreUnwatched runs the exact same sequence as
// CreateVeleroRestore but does not start resumeAfterInPlaceRestore: the caller
// says it runs its own loop to watch the restore to completion and resume Flux.
// Without this distinction a reconciler and the background goroutine would both
// drive the same resume — two mechanisms for one job, which is what the operator
// migration is meant to remove, not duplicate.
//
// Temporary: it exists only while the app and the operator coexist. Once the
// operator owns this path, resumeAfterInPlaceRestore and veleroResumeStates go
// away and this becomes the only behaviour.
func (s *Service) CreateVeleroRestoreUnwatched(ctx context.Context, actor models.Actor, name, env, backupName, targetName string) (VeleroRestoreView, error) {
	return s.createVeleroRestore(ctx, actor, name, env, backupName, targetName, true)
}

func (s *Service) createVeleroRestore(ctx context.Context, actor models.Actor, name, env, backupName, targetName string, callerOwnsFollowUp bool) (VeleroRestoreView, error) {
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
	if targetName != "" && targetName != name {
		return VeleroRestoreView{}, ErrCrossVClusterRestoreUnsupported
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return VeleroRestoreView{}, ErrK8sUnavailable
	}

	sourceNS := "vcluster-" + name
	targetNS := "vcluster-" + name
	inPlace := targetName == "" || targetName == name

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
		// Delete — not scale to 0 — the control-plane and etcd workloads: a
		// scaled-to-0-but-still-alive Deployment/StatefulSet races Velero's own
		// restore of that same object (existingResourcePolicy: update patches
		// its replica count back up) and recreates the pod itself, under a
		// name/identity Velero's node-agent isn't tracking. That is D3 (the
		// restore hangs forever waiting for a PodVolumeRestore that can never
		// complete, and wedges the whole Velero controller with it) and D2
		// (the volume comes back silently empty even from a backup that has
		// real data) — see QuiesceVClusterForInPlaceRestore's own comment for
		// how this was confirmed against a real Velero on the recette cell.
		topo, err := k8s.QuiesceVClusterForInPlaceRestore(ctx, name, 30*time.Second)
		if err != nil {
			s.abortInPlaceRestore(k8s, name)
			return VeleroRestoreView{}, &stageErr{ErrRestoreStageFailed, fmt.Errorf("suppression du plan de contrôle : %w", err)}
		}
		if err := k8s.DeleteVClusterPVCNamed(ctx, name, topo.PVCName); err != nil {
			// Volume déjà absent : on est DÉJÀ au-delà du point de non-retour, pas
			// en échec. C'est le cas du rejeu après un ErrRestoreStageFailedVolumeGone,
			// que le message d'erreur recommande justement. Le traiter comme un
			// échec d'étape faisait reprendre Flux sur un vcluster sans volume — le
			// StatefulSet recréait alors un PVC vide, exactement ce que la sentinelle
			// existe pour empêcher, contourné par le chemin de reprise documenté.
			if !apierrors.IsNotFound(err) {
				s.abortInPlaceRestore(k8s, name)
				return VeleroRestoreView{}, &stageErr{ErrRestoreStageFailed, fmt.Errorf("suppression du volume : %w", err)}
			}
			slog.Info("volume déjà absent, on poursuit : le point de non-retour était déjà franchi", "vcluster", name)
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
	// independent of the request. Unless the caller says it owns the follow-up,
	// in which case starting this would make two mechanisms race for the same
	// resume (see CreateVeleroRestoreUnwatched).
	if inPlace && !callerOwnsFollowUp {
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

// AbortInPlaceRestore resumes Flux on a vcluster left suspended and scaled to
// zero by an in-place restore sequence that stopped partway through. Admin only.
//
// CreateVeleroRestore already does this itself when it aborts within a single
// call. This is for the case it cannot cover: a caller that was interrupted
// mid-sequence and, on its way back, established from its own durable record
// that it stopped BEFORE the PVC was deleted — the volume is intact, so putting
// the vcluster back is the repair. Do not call it once the volume is gone:
// resuming Flux there lets the StatefulSet recreate an empty PVC and masks the
// failure (see ErrRestoreStageFailedVolumeGone).
func (s *Service) AbortInPlaceRestore(ctx context.Context, actor models.Actor, name, env string) error {
	if !actor.IsAdmin {
		return ErrForbidden
	}
	if !validName(name) {
		return ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return ErrK8sUnavailable
	}

	if err := k8s.SetFluxSuspend(ctx, name, false); err != nil {
		return err
	}
	audit.LogActor(actor.Username, "velero-restore-abort", name, env)
	return nil
}

// VeleroBackupRequested is the acknowledgement of a declaratively requested
// backup: the order is posted, the operator will carry it out.
type VeleroBackupRequested struct {
	RequestedAt string
	Name        string
	Env         string
}

// VeleroTriggerModeAnnotation selects the declarative path in
// cfg.VeleroTriggerMode. Anything else means the historical direct path.
const VeleroTriggerModeAnnotation = "annotation"

// newRequestNonce builds the value of a requestedAt annotation: readable,
// ordered, and compared as-is by the operator to decide whether a request is new.
//
// Nanosecond precision, not second: two requests within the same second would
// otherwise produce the same nonce, and the second one would be silently
// swallowed as "already handled". Harmless for a double-clicked backup, not
// harmless for a restore of a different backup.
func newRequestNonce() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// VeleroRestoreRequested is the acknowledgement of a declaratively requested
// restore.
type VeleroRestoreRequested struct {
	RequestedAt string
	FromBackup  string
	Target      string
	Name        string
	Env         string
	InPlace     bool
}

// RequestVeleroRestore asks for a restore by annotating the vcluster's
// VClusterVeleroOps marker. Admin only.
//
// It runs the same authorization and validation as CreateVeleroRestore, and
// writes the same audit line, at the moment a human asked. What it does NOT do is
// check that the backup is restorable: that check has to happen immediately
// before the destructive sequence, not minutes earlier when the order was
// posted — CreateVeleroRestore still does it, in the operator, where it belongs.
func (s *Service) RequestVeleroRestore(ctx context.Context, actor models.Actor, name, env, backupName, targetName string) (VeleroRestoreRequested, error) {
	if !actor.IsAdmin {
		return VeleroRestoreRequested{}, ErrForbidden
	}
	if backupName == "" {
		return VeleroRestoreRequested{}, ErrBackupNameRequired
	}
	if !validBackupName(backupName) {
		return VeleroRestoreRequested{}, ErrInvalidBackupName
	}
	if !validName(name) {
		return VeleroRestoreRequested{}, ErrInvalidName
	}
	if targetName != "" && !validName(targetName) {
		return VeleroRestoreRequested{}, ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return VeleroRestoreRequested{}, ErrK8sUnavailable
	}

	// A cross-vcluster restore is driven by the *target's* marker: the target
	// always exists, whereas the source may be long gone (design §10 bonus).
	markerVCluster := name
	if targetName != "" && targetName != name {
		markerVCluster = targetName
	}

	requestedAt := newRequestNonce()
	// La cible est TOUJOURS posée, y compris à la chaîne vide pour un in-place.
	//
	// Deux bugs en un si on l'omet. D'abord, une restauration croisée devenait
	// une restauration in-place sur la cible : l'opérateur lisait une annotation
	// absente, concluait `inPlace`, supprimait le volume de la CIBLE puis
	// restaurait un backup de la SOURCE dans le namespace de la cible — donc
	// rien, puisque le backup ne contient pas ce namespace. Perte du volume de la
	// cible, par une opération de routine.
	//
	// Ensuite, RequestVeleroOps fait un patch de FUSION, qui ne retire aucune
	// clé : une cible posée une fois resterait collée au marqueur, et tous les
	// in-place suivants deviendraient silencieusement des restaurations croisées
	// vers cette cible périmée. Écrire la chaîne vide écrase la précédente.
	annotations := map[string]string{
		v1alpha1.AnnRestoreRequestedAt: requestedAt,
		v1alpha1.AnnRestoreFromBackup:  backupName,
		v1alpha1.AnnRestoreTarget:      targetName,
		v1alpha1.AnnRestoreRequestedBy: actor.Username,
	}
	if err := k8s.RequestVeleroOps(ctx, markerVCluster, annotations); err != nil {
		return VeleroRestoreRequested{}, err
	}

	audit.LogActor(actor.Username, "velero-restore-request", name, env,
		"backup="+backupName, "marqueur="+markerVCluster)

	return VeleroRestoreRequested{
		RequestedAt: requestedAt,
		FromBackup:  backupName,
		Target:      targetName,
		Name:        markerVCluster,
		Env:         env,
		InPlace:     targetName == "" || targetName == name,
	}, nil
}

// StartVeleroRestore is the single entry point adapters call for a restore,
// picking the path from cfg.VeleroTriggerMode. Same reasoning as
// StartVeleroBackup: the switch belongs in one place.
//
// On the deferred path RestoreName is empty — the Velero Restore does not exist
// yet, the operator creates it. An adapter must therefore follow the marker's
// status rather than a restore name.
func (s *Service) StartVeleroRestore(ctx context.Context, actor models.Actor, name, env, backupName, targetName string) (VeleroRestoreView, error) {
	if s.cfg.VeleroTriggerMode != VeleroTriggerModeAnnotation {
		return s.CreateVeleroRestore(ctx, actor, name, env, backupName, targetName)
	}
	req, err := s.RequestVeleroRestore(ctx, actor, name, env, backupName, targetName)
	if err != nil {
		return VeleroRestoreView{}, err
	}
	return VeleroRestoreView{
		RestoreName: "", // deferred: the operator names it
		Phase:       "New",
		Name:        req.Name,
		Env:         req.Env,
		BackupName:  req.FromBackup,
		InPlace:     req.InPlace,
	}, nil
}

// VeleroBackupAck is what an adapter gets back from StartVeleroBackup. Exactly
// one of the two fields is set, and which one says who is doing the work:
// BackupName means Velero was called from this request, RequestedAt means the
// order was posted for the operator.
type VeleroBackupAck struct {
	BackupName  string
	RequestedAt string
	Name        string
	Env         string
}

// Deferred reports whether the backup was handed to the operator rather than
// created on the spot.
func (a VeleroBackupAck) Deferred() bool { return a.BackupName == "" }

// StartVeleroBackup is the single entry point adapters call for an on-demand
// backup. It picks the path from cfg.VeleroTriggerMode so the decision lives in
// one place — the alternative, an if in every adapter, is how a migration switch
// ends up half-applied.
//
// Migration switch: it disappears with the direct path once the operator owns
// this (docs/poc-operator-tech-decision.md §6).
func (s *Service) StartVeleroBackup(ctx context.Context, actor models.Actor, name, env string) (VeleroBackupAck, error) {
	if s.cfg.VeleroTriggerMode == VeleroTriggerModeAnnotation {
		res, err := s.RequestVeleroBackup(ctx, actor, name, env)
		if err != nil {
			return VeleroBackupAck{}, err
		}
		return VeleroBackupAck{RequestedAt: res.RequestedAt, Name: res.Name, Env: res.Env}, nil
	}
	res, err := s.TriggerVeleroBackup(ctx, actor, name, env)
	if err != nil {
		return VeleroBackupAck{}, err
	}
	return VeleroBackupAck{BackupName: res.BackupName, Name: res.Name, Env: res.Env}, nil
}

// RequestVeleroBackup asks for a backup by annotating the vcluster's
// VClusterVeleroOps marker, instead of calling Velero itself. Admin only.
//
// This is the declarative entry point (design §6): the authorization check and
// the audit line stay here, at the moment a human asked and with their identity,
// while the execution — and its own audit line — happens in the operator. The
// two together answer "who asked, when" and "what ran, when", which one
// synchronous call could never separate.
//
// It returns as soon as the order is posted. That is not a regression in
// responsiveness: TriggerVeleroBackup already returned before Velero had
// finished, and the UI already polled for the phase.
func (s *Service) RequestVeleroBackup(ctx context.Context, actor models.Actor, name, env string) (VeleroBackupRequested, error) {
	if !actor.IsAdmin {
		return VeleroBackupRequested{}, ErrForbidden
	}
	if !validName(name) {
		return VeleroBackupRequested{}, ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return VeleroBackupRequested{}, ErrK8sUnavailable
	}

	requestedAt := newRequestNonce()
	if err := k8s.RequestVeleroOps(ctx, name, map[string]string{
		v1alpha1.AnnBackupRequestedAt: requestedAt,
	}); err != nil {
		return VeleroBackupRequested{}, err
	}

	audit.LogActor(actor.Username, "velero-backup-request", name, env, "requestedAt="+requestedAt)
	return VeleroBackupRequested{RequestedAt: requestedAt, Name: name, Env: env}, nil
}

// GetVeleroOpsRestoreStatus reports the restore status the operator publishes on
// a vcluster's marker, in the same shape as GetVeleroRestoreStatus so an adapter
// can render both with one template. Read-only, no privilege required.
//
// The two differ on one point: here the service does not attempt the Flux resume
// itself. The operator owns that, and its own reconcile loop retries it — a
// second mechanism poking at it from a browser poll is exactly what this
// migration removes.
func (s *Service) GetVeleroOpsRestoreStatus(ctx context.Context, name, env string) (VeleroRestoreStatusView, error) {
	if !validName(name) {
		return VeleroRestoreStatusView{}, ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return VeleroRestoreStatusView{}, ErrK8sUnavailable
	}

	st, err := k8s.GetVeleroOpsRestoreState(ctx, name)
	if err != nil {
		return VeleroRestoreStatusView{}, err
	}
	if !st.Found || st.Phase == "" {
		// The order is posted but the operator has not written anything yet.
		// "New" keeps the adapter polling instead of declaring an outcome.
		return VeleroRestoreStatusView{Name: name, Env: env, Phase: "New"}, nil
	}

	return VeleroRestoreStatusView{
		RestoreName:     st.RestoreName,
		Phase:           st.Phase,
		Name:            name,
		Env:             env,
		InPlace:         st.InPlace,
		ResumePending:   st.ResumePending,
		ResumeFailed:    st.ResumeFailed,
		ResumeError:     st.ResumeError,
		VolumeDestroyed: st.VolumeDestroyed,
	}, nil
}

// InterruptedRestoreView is what the cluster says about a restore sequence that
// was interrupted partway through — enough to decide what to do about it without
// having had to write down the progress beforehand.
type InterruptedRestoreView struct {
	// VolumeGone is true when the data PVC is absent or already being deleted:
	// the sequence got past the point of no return, so resuming Flux would let
	// the StatefulSet recreate an empty volume and mask the loss.
	VolumeGone bool
	// ActiveRestoreName is a non-terminal Velero Restore targeting this
	// vcluster, if there is one. Its presence means the sequence did reach the
	// end — the restore is running and only needs following, not repairing.
	ActiveRestoreName string
	// ActiveRestorePhase is that restore's phase, empty if there is none.
	ActiveRestorePhase string
}

// InspectInterruptedRestore observes the state an interrupted in-place restore
// left behind. Read-only, no privilege required.
//
// It replaces the alternative of persisting each step as it happens: the two
// facts that matter are already in the cluster, and reading them is more
// reliable than trusting a record written by a process that then died — a record
// can be stale or optimistic, the PVC cannot.
func (s *Service) InspectInterruptedRestore(ctx context.Context, name, env string) (InterruptedRestoreView, error) {
	if !validName(name) {
		return InterruptedRestoreView{}, ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return InterruptedRestoreView{}, ErrK8sUnavailable
	}

	exists, deleting, err := k8s.GetVClusterPVCState(ctx, name)
	if err != nil {
		return InterruptedRestoreView{}, err
	}

	view := InterruptedRestoreView{VolumeGone: !exists || deleting}

	// A restore may have been created just before the interruption, without the
	// caller ever learning its name. Finding it here is what stops the recovery
	// from mistaking a running restore for a lost volume.
	active, err := k8s.ListActiveVeleroRestores(ctx, name, s.cfg.VeleroNamespace)
	if err != nil {
		return InterruptedRestoreView{}, err
	}
	if len(active) > 0 {
		view.ActiveRestoreName = active[0].Name
		view.ActiveRestorePhase = active[0].Phase
	}
	return view, nil
}

// GetVeleroBackupPhase returns the phase of a single backup. Read-only, no
// privilege required, like GetVeleroBackups — which is what a caller had to use
// to answer this question until now, listing every backup of the vcluster to
// look at one of them.
func (s *Service) GetVeleroBackupPhase(ctx context.Context, backup, env string) (string, error) {
	if backup == "" {
		return "", ErrBackupNameRequired
	}
	if !validBackupName(backup) {
		return "", ErrInvalidBackupName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return "", ErrK8sUnavailable
	}
	return k8s.GetVeleroBackupPhase(ctx, backup, s.cfg.VeleroNamespace)
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

	interval := s.resumeWatchInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	terminalSeen := false
	for {
		select {
		case <-ctx.Done():
			slog.Warn("giving up waiting on the restore/resume, resuming flux as a last resort", "restore", restoreName, "vcluster", name)
			err := k8s.SetFluxSuspend(context.Background(), name, false)
			if err != nil {
				slog.Error("could not resume flux after timeout", "vcluster", name, "err", err)
			}
			s.resolveVeleroResume(restoreName, err != nil, errText(err))
			return
		case <-ticker.C:
			if !terminalSeen {
				phase, err := k8s.GetRestoreStatus(ctx, restoreName, veleroNamespace)
				if err != nil {
					slog.Warn("polling restore failed", "restore", restoreName, "err", err)
					continue
				}
				if !IsTerminalRestorePhase(phase) {
					continue
				}
				terminalSeen = true
				slog.Info("restore reached terminal phase", "restore", restoreName, "phase", phase, "vcluster", name)
			}
			// Terminal phase reached: keep retrying the resume itself until it
			// works, or the deadline above gives up on our behalf.
			if err := k8s.SetFluxSuspend(context.Background(), name, false); err != nil {
				slog.Warn("could not resume flux yet, retrying", "vcluster", name, "err", err)
				continue
			}
			slog.Info("flux resumed after restore", "restore", restoreName, "vcluster", name)
			s.resolveVeleroResume(restoreName, false, "")
			return
		}
	}
}

// VeleroRestoreStatusView is a polled Velero restore's current phase.
//
// For an in-place restore, ResumePending/ResumeFailed/ResumeError are the
// data-safety contract with the adapters: once Phase is terminal, the UI
// must not say "Flux repris" unless both ResumePending and ResumeFailed are
// false. ResumePending means the resume hasn't been settled yet — the
// background watcher is still retrying — and the adapter must keep polling
// rather than show a final message. ResumeFailed means the watcher gave up
// for good: the vcluster is stuck suspended and at zero replicas, a real
// problem the operator needs to know about. VolumeDestroyed flags a Failed
// in-place restore: the vcluster's PVC was deleted before the restore ran
// (the same root cause as ErrRestoreStageFailedVolumeGone, just surfacing
// later), so even once Flux is back the vcluster comes up on an empty
// volume — what it actually needs is a new restore.
type VeleroRestoreStatusView struct {
	RestoreName     string
	Phase           string
	Name            string
	Env             string
	InPlace         bool
	ResumePending   bool
	ResumeFailed    bool
	ResumeError     string
	VolumeDestroyed bool
}

// GetVeleroRestoreStatus polls the phase of a Velero restore. For an in-place
// restore that has reached a terminal phase, it also reports whether Flux
// has been resumed — first checking veleroResumeResult in case the
// background watcher (started by CreateVeleroRestore) already settled it,
// and if not, making its own attempt (whichever side gets there first wins;
// resuming twice is harmless, SetFluxSuspend is idempotent). A failed
// attempt here is reported as ResumePending rather than ResumeFailed: the
// background watcher keeps retrying independently, so a transient failure
// must not freeze the UI on a stale "resume failed" message while the
// vcluster is actually fine moments later.
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

	var resumePending, resumeFailed bool
	var resumeErrMsg string
	if inPlace && IsTerminalRestorePhase(phase) {
		if state, resolved := s.veleroResumeResult(restoreName); resolved {
			resumeFailed = state.failed
			resumeErrMsg = state.errMsg
		} else if resumeErr := k8s.SetFluxSuspend(ctx, name, false); resumeErr != nil {
			slog.Warn("could not resume flux yet, background watcher will retry", "vcluster", name, "err", resumeErr)
			resumePending = true
		} else {
			s.resolveVeleroResume(restoreName, false, "")
		}
	}

	return VeleroRestoreStatusView{
		RestoreName:     restoreName,
		Phase:           phase,
		Name:            name,
		Env:             env,
		InPlace:         inPlace,
		ResumePending:   resumePending,
		ResumeFailed:    resumeFailed,
		ResumeError:     resumeErrMsg,
		VolumeDestroyed: inPlace && phase == "Failed",
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
	// Seul point d'entrée Velero qui ne validait pas le nom, alors qu'il sert à
	// construire le namespace sauvegardé par concaténation.
	if !validName(name) {
		return VeleroBackupCreated{}, ErrInvalidName
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
