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
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/veleroops"
)

// RequeueInterval is how often an in-flight backup or restore is polled. Matches
// the 10s ticker of the goroutine this loop replaces.
const RequeueInterval = 10 * time.Second

// SystemActor is the actor the controller passes to the service. IsAdmin is
// true because the human authorization check already happened once, in the
// service, when the annotation was patched (design §6 point 1). This is a
// technical identity, NOT a bypass — the audit trail keeps both lines: who
// asked, and what the operator executed.
var SystemActor = models.Actor{Username: "vcluster-operator", IsAdmin: true}

// VeleroOpsReconciler reconciles VClusterVeleroOps markers.
type VeleroOpsReconciler struct {
	client.Client

	// Ops is the service seam — *service.Service in production.
	Ops veleroops.Ops

	// Cell names the host cluster this operator reconciles (ADR-002). It is not
	// used to pick a client — the operator has exactly one — but it labels the
	// audit trail and the metrics, and those must not claim one cell's name on
	// another. Empty falls back to the service's historical default, which is
	// exactly the mistake to avoid: set it.
	//
	// It is passed as the service's `env` parameter, which still carries the old
	// prod/preprod vocabulary. That mapping is the migration seam: the cell name
	// is what identifies the host cluster from now on, and `env` is what the
	// service has not been renamed to yet.
	Cell string
}

// Reconcile is level-triggered: everything it does comes from the object's
// annotations and status plus what it can read back from the cluster, never from
// in-process memory. That is the property being demonstrated — a restart loses
// nothing.
func (r *VeleroOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ops v1alpha1.VClusterVeleroOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// La garde de placement passe avant la moindre lecture d'annotation : le
	// marqueur doit d'abord prouver qu'il parle du vcluster où il vit.
	// Pas d'erreur retournée — rien ne sera réessayé, et c'est voulu : un objet
	// refusé le restera tant qu'il n'aura pas bougé, et le requeuer en boucle
	// donnerait à n'importe qui un moyen de faire tourner l'opérateur à vide.
	if reason := markerMisplaced(&ops); reason != "" {
		refuseMarker(&ops, reason)
		return ctrl.Result{}, r.Status().Update(ctx, &ops)
	}

	// Restore before backup: it is the destructive path, and an interrupted one
	// must not wait behind a backup poll.
	restoreRequeue, restoreErr := r.reconcileRestore(ctx, &ops)
	backupRequeue, backupErr := r.reconcileBackup(ctx, &ops)

	// Every write goes through the /status subresource, never Update on the whole
	// object — the annotations belong to the requester (the app), the status
	// belongs to the controller.
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
		if err := r.Status().Update(ctx, ops); err != nil {
			return 0, err
		}

		created, err := r.Ops.TriggerVeleroBackup(ctx, SystemActor, ops.VClusterName(), r.Cell)
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
		return RequeueInterval, nil
	}

	if st.BackupName == "" || veleroops.IsTerminalBackupPhase(st.Phase) {
		return 0, nil
	}

	phase, err := r.Ops.GetVeleroBackupPhase(ctx, st.BackupName, r.Cell)
	if err != nil {
		return RequeueInterval, err
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
	return RequeueInterval, nil
}

// reconcileRestore handles the restore annotation and, crucially, the recovery
// of a sequence interrupted by a restart.
func (r *VeleroOpsReconciler) reconcileRestore(ctx context.Context, ops *v1alpha1.VClusterVeleroOps) (time.Duration, error) {
	st := &ops.Status.Restore
	requestedAt := ops.Annotations[v1alpha1.AnnRestoreRequestedAt]

	if requestedAt != "" && requestedAt != st.LastHandledRequestedAt {
		return r.startRestore(ctx, ops, requestedAt)
	}
	if st.RestoreName != "" {
		return r.followRestore(ctx, ops)
	}
	// A request was reserved but no restore name was ever recorded: the sequence
	// was interrupted somewhere. Ask the cluster where.
	if st.LastHandledRequestedAt != "" && st.Phase == "New" {
		return r.recoverInterrupted(ctx, ops)
	}
	return 0, nil
}

func (r *VeleroOpsReconciler) startRestore(ctx context.Context, ops *v1alpha1.VClusterVeleroOps, requestedAt string) (time.Duration, error) {
	st := &ops.Status.Restore
	name := ops.VClusterName()

	// The guard the imperative path lacks: never let a second sequence start
	// while the first is still running.
	if st.RestoreName != "" && !service.IsTerminalRestorePhase(st.Phase) {
		setCond(ops, v1alpha1.CondRestoreRejectedBusy, metav1.ConditionTrue, "AlreadyRunning",
			fmt.Sprintf("restauration %s en cours (phase %s) : demande %s différée", st.RestoreName, st.Phase, requestedAt))
		// The annotation is NOT consumed, so the request is honoured once the
		// current restore settles. Deferred, not dropped.
		return RequeueInterval, nil
	}

	fromBackup := ops.Annotations[v1alpha1.AnnRestoreFromBackup]
	target := ops.Annotations[v1alpha1.AnnRestoreTarget]
	inPlace := target == "" || target == name

	// Reserve the request before doing anything destructive.
	*st = v1alpha1.RestoreOpsStatus{
		LastHandledRequestedAt: requestedAt,
		FromBackup:             fromBackup,
		Target:                 target,
		InPlace:                inPlace,
		Phase:                  "New",
	}
	apimeta.RemoveStatusCondition(&ops.Status.Conditions, v1alpha1.CondRestoreRejectedBusy)
	apimeta.RemoveStatusCondition(&ops.Status.Conditions, v1alpha1.CondRestoreNeedsRetry)
	setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionTrue, "Requested",
		fmt.Sprintf("restauration de %s demandée", fromBackup))
	if err := r.Status().Update(ctx, ops); err != nil {
		return 0, err
	}

	view, err := r.Ops.CreateVeleroRestoreUnwatched(ctx, SystemActor, name, r.Cell, fromBackup, target)
	if err != nil {
		return 0, r.recordRestoreFailure(ops, err)
	}

	st.RestoreName = view.RestoreName
	st.Phase = view.Phase
	st.InPlace = view.InPlace
	return RequeueInterval, nil
}

// recoverInterrupted decides what an interrupted sequence needs, from what the
// cluster says rather than from a record the dead process left behind.
func (r *VeleroOpsReconciler) recoverInterrupted(ctx context.Context, ops *v1alpha1.VClusterVeleroOps) (time.Duration, error) {
	st := &ops.Status.Restore
	name := ops.VClusterName()

	view, err := r.Ops.InspectInterruptedRestore(ctx, name, r.Cell)
	if err != nil {
		return RequeueInterval, err
	}

	switch {
	case view.ActiveRestoreName != "":
		// The sequence did reach the end; only the bookkeeping was lost. Adopt
		// the restore and follow it — if we treated this as a lost volume we
		// would leave Flux suspended forever on a restore that is about to work.
		st.RestoreName = view.ActiveRestoreName
		st.Phase = view.ActiveRestorePhase
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionTrue, "Adopted",
			fmt.Sprintf("restauration %s retrouvée après interruption, suivi reprisi", view.ActiveRestoreName))
		return RequeueInterval, nil

	case view.VolumeGone:
		// Past the point of no return with no restore to show for it. Resuming
		// Flux here would recreate an empty PVC and mask the failure — leave it
		// suspended. Only a new restore request fixes this.
		st.VolumeDestroyed = true
		st.Phase = "Failed"
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionFalse, "Interrupted",
			"séquence interrompue après suppression du volume")
		setCond(ops, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "VolumeGoneNoRestore",
			"volume supprimé, aucun restore en cours : vcluster laissé suspendu volontairement, un nouveau restore est nécessaire")
		return 0, nil

	default:
		// Volume intact: the repair is to put the vcluster back.
		if err := r.Ops.AbortInPlaceRestore(ctx, SystemActor, name, r.Cell); err != nil {
			setCond(ops, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "AbortFailed",
				"reprise de Flux après séquence interrompue : "+err.Error())
			return RequeueInterval, err
		}
		st.Phase = "Failed"
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionFalse, "Aborted",
			"séquence interrompue avant suppression du volume, Flux repris")
		setCond(ops, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "InterruptedBeforePointOfNoReturn",
			"volume intact, Flux repris : relancer un restore quand vous voulez")
		return 0, nil
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
// a goroutine with a 2h timeout and no memory of a restart. Here the deadline
// lives in status, so it survives one.
func (r *VeleroOpsReconciler) followRestore(ctx context.Context, ops *v1alpha1.VClusterVeleroOps) (time.Duration, error) {
	st := &ops.Status.Restore
	if service.IsTerminalRestorePhase(st.Phase) && !st.ResumePending {
		return 0, nil
	}

	view, err := r.Ops.GetVeleroRestoreStatus(ctx, ops.VClusterName(), st.RestoreName, r.Cell, st.InPlace)
	if err != nil {
		log.FromContext(ctx).Error(err, "poll du restore", "restore", st.RestoreName)
		return RequeueInterval, err
	}

	st.Phase = view.Phase
	st.ResumePending = view.ResumePending
	st.ResumeFailed = view.ResumeFailed
	st.ResumeError = view.ResumeError
	st.VolumeDestroyed = view.VolumeDestroyed

	if !service.IsTerminalRestorePhase(view.Phase) {
		setCond(ops, v1alpha1.CondRestoreInProgress, metav1.ConditionTrue, "InProgress",
			fmt.Sprintf("restauration %s en phase %s", st.RestoreName, view.Phase))
		return RequeueInterval, nil
	}

	if st.FirstTerminalAt == nil {
		now := metav1.Now()
		st.FirstTerminalAt = &now
	}

	if view.ResumePending {
		// Bounded, unlike an unqualified "keep retrying": a resume that never
		// works must end up reported, not retried in silence forever.
		if time.Since(st.FirstTerminalAt.Time) > v1alpha1.ResumeGiveUpAfter {
			st.ResumeFailed = true
			st.ResumePending = false
			st.ResumeError = fmt.Sprintf("Flux toujours pas repris après %s", v1alpha1.ResumeGiveUpAfter)
			setCond(ops, v1alpha1.CondFluxResumePending, metav1.ConditionTrue, "ResumeGaveUp",
				"reprise de Flux abandonnée : "+st.ResumeError+" — le vcluster est suspendu et à zéro réplique, intervention nécessaire")
			return 0, nil
		}
		setCond(ops, v1alpha1.CondFluxResumePending, metav1.ConditionTrue, "ResumePending",
			"phase terminale atteinte, reprise de Flux pas encore confirmée")
		return RequeueInterval, nil
	}

	if view.ResumeFailed {
		setCond(ops, v1alpha1.CondFluxResumePending, metav1.ConditionTrue, "ResumeFailed",
			"reprise de Flux en échec : "+view.ResumeError)
	} else {
		setCond(ops, v1alpha1.CondFluxResumePending, metav1.ConditionFalse, "Resumed", "Flux repris")
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

func setCond(ops *v1alpha1.VClusterVeleroOps, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&ops.Status.Conditions, metav1.Condition{
		Type:    condType,
		Status:  status,
		Reason:  reason,
		Message: message,
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
// polling for events.
//
// MaxConcurrentReconciles is above 1 because a restore reconcile is synchronous
// and long — it waits for pods to terminate — and with the default of 1 it would
// freeze the backup polling of every other marker while it runs.
func (r *VeleroOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VClusterVeleroOps{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Named("vclusterveleroops").
		Complete(r)
}
