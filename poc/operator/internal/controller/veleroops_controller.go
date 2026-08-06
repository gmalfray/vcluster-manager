// Package controller holds the POC reconciler for the VClusterVeleroOps
// marker: the concrete answer to "can a controller-runtime reconcile loop
// replace the hand-rolled goroutines (resumeAfterInPlaceRestore,
// veleroResumeStates) without touching the business logic?".
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"

	"github.com/gmalfray/vcluster-manager/poc/operator/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/poc/operator/internal/veleroops"
)

// DefaultRequeueInterval matches the 10s ticker of the goroutine this loop
// replaces (service.resumeAfterInPlaceRestore).
const DefaultRequeueInterval = 10 * time.Second

// SystemActor is the actor the controller passes to the service. IsAdmin is
// true because the human authorization check already happened once, in the
// service, when the annotation was patched (design §6 point 1). This is a
// technical identity, NOT a bypass — the audit trail keeps both lines: who
// asked, and what the operator executed.
var SystemActor = models.Actor{Username: "vcluster-operator", IsAdmin: true}

// VeleroOpsReconciler reconciles VClusterVeleroOps markers.
type VeleroOpsReconciler struct {
	client.Client

	// Ops is the service seam. In production this is *service.Service (plus
	// the two additive methods listed in veleroops/seam_assert.go).
	Ops veleroops.Ops

	// RequeueInterval is how often an in-flight backup/restore is polled.
	// Zero means DefaultRequeueInterval.
	RequeueInterval time.Duration
}

// Reconcile is level-triggered: it derives everything it does from the object's
// annotations and status, never from in-process memory. That is the property
// being demonstrated — a restart loses nothing.
func (r *VeleroOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ops v1alpha1.VClusterVeleroOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ops.Status.ObservedGeneration = ops.Generation

	// Restore before backup: it is the destructive path, and an interrupted one
	// must not wait behind a backup poll.
	restoreRequeue, restoreErr := r.reconcileRestore(ctx, &ops)
	backupRequeue, backupErr := r.reconcileBackup(ctx, &ops)

	// Flush whatever the two paths left in memory. Every write goes through the
	// /status subresource, never Update on the whole object — the annotations
	// belong to the requester (the app), the status belongs to the controller.
	if err := r.Status().Update(ctx, &ops); err != nil {
		return ctrl.Result{}, err
	}

	if err := errors.Join(restoreErr, backupErr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: soonest(restoreRequeue, backupRequeue)}, nil
}

// reconcileBackup handles the backup annotation. Returns the requeue delay it
// wants (0 = nothing pending).
func (r *VeleroOpsReconciler) reconcileBackup(ctx context.Context, ops *v1alpha1.VClusterVeleroOps) (time.Duration, error) {
	st := &ops.Status.Backup
	requestedAt := ops.Annotations[v1alpha1.AnnBackupRequestedAt]

	if requestedAt != "" && requestedAt != st.LastHandledRequestedAt {
		// Consume the request BEFORE acting on it. At-most-once, on purpose: a
		// crash between here and the Velero call loses the request, which is
		// the safe direction — the alternative re-runs it on every restart.
		st.LastHandledRequestedAt = requestedAt
		st.BackupName = ""
		st.Phase = "New"
		st.RequestedTTL = ops.Annotations[v1alpha1.AnnBackupTTL]
		st.TTLHonoured = st.RequestedTTL == ""
		if err := r.Status().Update(ctx, ops); err != nil {
			return 0, err
		}

		created, err := r.Ops.TriggerVeleroBackup(ctx, SystemActor, ops.Spec.VClusterName, ops.Spec.Env)
		if err != nil {
			st.Phase = "Failed"
			setCond(ops, v1alpha1.CondBackupCompleted, metav1.ConditionFalse, "TriggerFailed", err.Error())
			// Not returned as a reconcile error: retrying a failed *trigger*
			// on its own would need a new request anyway (at-most-once above).
			return 0, nil
		}
		st.BackupName = created.BackupName
		setCond(ops, v1alpha1.CondBackupCompleted, metav1.ConditionUnknown, "InProgress",
			fmt.Sprintf("backup %s en cours", created.BackupName))
		return r.requeue(), nil
	}

	if st.BackupName == "" || veleroops.IsTerminalBackupPhase(st.Phase) {
		return 0, nil
	}

	phase, err := r.Ops.GetVeleroBackupPhase(ctx, st.BackupName, ops.Spec.Env)
	if err != nil {
		return r.requeue(), err
	}
	st.Phase = phase
	switch {
	case phase == "Completed":
		setCond(ops, v1alpha1.CondBackupCompleted, metav1.ConditionTrue, "Completed",
			fmt.Sprintf("backup %s terminé", st.BackupName))
		return 0, nil
	case veleroops.IsTerminalBackupPhase(phase):
		setCond(ops, v1alpha1.CondBackupCompleted, metav1.ConditionFalse, "Failed",
			fmt.Sprintf("backup %s en phase %s", st.BackupName, phase))
		return 0, nil
	}
	return r.requeue(), nil
}

// reconcileRestore handles the restore annotation and, crucially, the recovery
// of a sequence interrupted by a restart.
func (r *VeleroOpsReconciler) reconcileRestore(ctx context.Context, ops *v1alpha1.VClusterVeleroOps) (time.Duration, error) {
	st := &ops.Status.Restore
	requestedAt := ops.Annotations[v1alpha1.AnnRestoreRequestedAt]

	if requestedAt != "" && requestedAt != st.LastHandledRequestedAt {
		return r.startRestore(ctx, ops, requestedAt)
	}

	// No new request: either follow an in-flight restore, or repair one that a
	// restart interrupted.
	switch {
	case st.RestoreName != "":
		return r.followRestore(ctx, ops)
	case st.Stage == v1alpha1.StagePVCDeleted:
		// Past the point of no return with no Restore object to show for it.
		// Resuming Flux here would recreate an empty PVC and mask the failure —
		// leave it suspended, exactly like ErrRestoreStageFailedVolumeGone does
		// in the synchronous path. Only a new restore request fixes this.
		st.VolumeDestroyed = true
		st.Phase = "Failed"
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionFalse, "Interrupted",
			"séquence interrompue après suppression du volume")
		setCond(ops, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "VolumeGoneNoRestore",
			"volume supprimé, aucun Restore créé : vcluster laissé suspendu volontairement, un nouveau restore est nécessaire")
		return 0, nil
	case st.Stage == v1alpha1.StageFluxSuspended, st.Stage == v1alpha1.StageScaledDown:
		// Before the point of no return: the volume is intact, so the repair is
		// to put the vcluster back — what abortInPlaceRestore does today, only
		// reachable here because the stage was persisted before each step.
		if err := r.Ops.AbortInPlaceRestore(ctx, SystemActor, ops.Spec.VClusterName, ops.Spec.Env); err != nil {
			setCond(ops, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "AbortFailed",
				"reprise de Flux après séquence interrompue : "+err.Error())
			return r.requeue(), err
		}
		st.Stage = ""
		st.Phase = "Failed"
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionFalse, "Aborted",
			"séquence interrompue avant suppression du volume, Flux repris")
		setCond(ops, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "InterruptedBeforePointOfNoReturn",
			"volume intact, Flux repris : relancer un restore quand vous voulez")
		return 0, nil
	}
	return 0, nil
}

func (r *VeleroOpsReconciler) startRestore(ctx context.Context, ops *v1alpha1.VClusterVeleroOps, requestedAt string) (time.Duration, error) {
	st := &ops.Status.Restore

	// The guard the imperative path lacks: never let a second sequence start
	// while the first is still running.
	if st.RestoreName != "" && !service.IsTerminalRestorePhase(st.Phase) {
		setCond(ops, v1alpha1.CondRestoreRejectedBusy, metav1.ConditionTrue, "AlreadyRunning",
			fmt.Sprintf("restauration %s en cours (phase %s) : demande %s différée", st.RestoreName, st.Phase, requestedAt))
		// The annotation is NOT consumed, so the request is honoured once the
		// current restore settles. Deferred, not dropped — see the doc's
		// "sémantique refus vs report" finding.
		return r.requeue(), nil
	}

	fromBackup := ops.Annotations[v1alpha1.AnnRestoreFromBackup]
	target := ops.Annotations[v1alpha1.AnnRestoreTarget]
	inPlace := target == "" || target == ops.Spec.VClusterName

	// Reserve the request before doing anything destructive.
	st.LastHandledRequestedAt = requestedAt
	st.FromBackup = fromBackup
	st.Target = target
	st.InPlace = inPlace
	st.RestoreName = ""
	st.Stage = ""
	st.Phase = "New"
	st.ResumePending = false
	st.ResumeFailed = false
	st.ResumeError = ""
	st.VolumeDestroyed = false
	apimeta.RemoveStatusCondition(&ops.Status.Conditions, v1alpha1.CondRestoreRejectedBusy)
	apimeta.RemoveStatusCondition(&ops.Status.Conditions, v1alpha1.CondRestoreNeedsRetry)
	setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionTrue, "Requested",
		fmt.Sprintf("restauration de %s demandée", fromBackup))
	if err := r.Status().Update(ctx, ops); err != nil {
		return 0, err
	}

	view, err := r.Ops.CreateVeleroRestoreWithHooks(ctx, SystemActor,
		ops.Spec.VClusterName, ops.Spec.Env, fromBackup, target,
		service.RestoreHooks{OnStage: r.stageWriter(ops), OwnsFollowUp: true})
	if err != nil {
		return 0, r.recordRestoreFailure(ops, err)
	}

	st.RestoreName = view.RestoreName
	st.Phase = view.Phase
	st.InPlace = view.InPlace
	st.Stage = v1alpha1.StageRestoreCreated
	return r.requeue(), nil
}

// stageWriter persists a stage through the /status subresource before the step
// it announces runs. A failure here aborts the sequence rather than letting it
// proceed unrecorded.
func (r *VeleroOpsReconciler) stageWriter(ops *v1alpha1.VClusterVeleroOps) func(context.Context, service.RestoreStage) error {
	return func(ctx context.Context, stage service.RestoreStage) error {
		ops.Status.Restore.Stage = v1alpha1.RestoreStage(stage)
		if err := r.Status().Update(ctx, ops); err != nil {
			return fmt.Errorf("persistance de l'étape %s : %w", stage, err)
		}
		return nil
	}
}

// recordRestoreFailure maps the service's sentinels onto conditions. The two
// sentinels carry the data-safety decision, so they are branched on explicitly
// rather than collapsed into one "restore failed".
func (r *VeleroOpsReconciler) recordRestoreFailure(ops *v1alpha1.VClusterVeleroOps, err error) error {
	st := &ops.Status.Restore
	st.Phase = "Failed"
	setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionFalse, "StageFailed", err.Error())

	switch {
	case errors.Is(err, service.ErrRestoreStageFailedVolumeGone):
		// Volume gone, no Restore object: the service already left Flux
		// suspended on purpose. Say so, loudly.
		st.VolumeDestroyed = true
		setCond(ops, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "VolumeGone",
			"volume supprimé et Restore non créé : vcluster laissé suspendu, un nouveau restore est nécessaire — "+err.Error())
	case errors.Is(err, service.ErrRestoreStageFailed):
		// Failed before the point of no return; the service resumed Flux itself.
		// Clear the stage: it may say PVCDeleted (announced before the delete that
		// then failed), and leaving it there would make the next reconcile treat
		// this as an interrupted sequence past the point of no return and report
		// a volume loss that did not happen. The failure is already recorded in
		// the conditions; the stage marker has done its job.
		st.Stage = ""
		setCond(ops, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "StageFailed",
			"séquence abandonnée avant suppression du volume, Flux repris par le service — "+err.Error())
	case errors.Is(err, service.ErrForbidden):
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionFalse, "Forbidden", err.Error())
	}
	// Not a reconcile error: retrying a destructive sequence automatically is
	// exactly what we do not want. A new requestedAt is the retry.
	return nil
}

// followRestore polls a live restore and, for an in-place one, keeps requeueing
// until Flux is actually back — the job resumeAfterInPlaceRestore does today in
// a goroutine with a 2h timeout and no memory of a restart.
func (r *VeleroOpsReconciler) followRestore(ctx context.Context, ops *v1alpha1.VClusterVeleroOps) (time.Duration, error) {
	st := &ops.Status.Restore
	if service.IsTerminalRestorePhase(st.Phase) && !st.ResumePending {
		return 0, nil
	}

	view, err := r.Ops.GetVeleroRestoreStatus(ctx, ops.Spec.VClusterName, st.RestoreName, ops.Spec.Env, st.InPlace)
	if err != nil {
		log.FromContext(ctx).Error(err, "poll du restore", "restore", st.RestoreName)
		return r.requeue(), err
	}

	st.Phase = view.Phase
	st.ResumePending = view.ResumePending
	st.ResumeFailed = view.ResumeFailed
	st.ResumeError = view.ResumeError
	st.VolumeDestroyed = view.VolumeDestroyed

	if !service.IsTerminalRestorePhase(view.Phase) {
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionTrue, "InProgress",
			fmt.Sprintf("restauration %s en phase %s", st.RestoreName, view.Phase))
		return r.requeue(), nil
	}

	switch {
	case view.ResumePending:
		setCond(ops, v1alpha1.CondFluxResumePending, metav1.ConditionTrue, "ResumePending",
			"phase terminale atteinte, reprise de Flux pas encore confirmée")
		return r.requeue(), nil
	case view.ResumeFailed:
		setCond(ops, v1alpha1.CondFluxResumePending, metav1.ConditionTrue, "ResumeFailed",
			"reprise de Flux en échec : "+view.ResumeError)
	default:
		setCond(ops, v1alpha1.CondFluxResumePending, metav1.ConditionFalse, "Resumed",
			"Flux repris")
	}

	if view.Phase == "Completed" {
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionFalse, "Completed",
			fmt.Sprintf("restauration %s terminée", st.RestoreName))
	} else {
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionFalse, "Failed",
			fmt.Sprintf("restauration %s en phase %s", st.RestoreName, view.Phase))
	}
	if view.VolumeDestroyed {
		setCond(ops, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "VolumeDestroyed",
			"restauration in-place en échec après suppression du volume : un nouveau restore est nécessaire")
	}
	return 0, nil
}

func (r *VeleroOpsReconciler) requeue() time.Duration {
	if r.RequeueInterval > 0 {
		return r.RequeueInterval
	}
	return DefaultRequeueInterval
}

func setCond(ops *v1alpha1.VClusterVeleroOps, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&ops.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ops.Generation,
	})
}

func soonest(a, b time.Duration) time.Duration {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// SetupWithManager wires the reconciler. Watching only the marker is enough for
// the POC; the real thing may also watch Velero Backup/Restore objects to trade
// polling for events (an open point in the design).
func (r *VeleroOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VClusterVeleroOps{}).
		Named("vclusterveleroops").
		Complete(r)
}
