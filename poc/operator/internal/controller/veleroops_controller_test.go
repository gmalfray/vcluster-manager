package controller

import (
	"context"
	"maps"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/gmalfray/vcluster-manager/internal/service"

	"github.com/gmalfray/vcluster-manager/poc/operator/api/v1alpha1"
)

// Property 1 — a backup request is acted on exactly once, however many times
// the object is reconciled. This is the whole reason for the annotation +
// lastHandledRequestedAt pair instead of a spec field.
func TestBackupRequestIsHandledExactlyOnce(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{backupName: "manual-demo-1", backupPhases: []string{"InProgress", "Completed"}}
	r := newReconciler(ops)

	obj := newMarker(t, ctx, "backup-once", map[string]string{
		v1alpha1.AnnBackupRequestedAt: "2026-08-06T14:32:00Z",
	})

	res, err := r.Reconcile(ctx, reqFor(obj))
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if res.RequeueAfter != RequeueInterval {
		t.Fatalf("requeue attendu %s, obtenu %s", RequeueInterval, res.RequeueAfter)
	}
	got := fetch(t, ctx, obj)
	if got.Status.Backup.LastHandledRequestedAt != "2026-08-06T14:32:00Z" {
		t.Fatalf("lastHandledRequestedAt = %q", got.Status.Backup.LastHandledRequestedAt)
	}
	if got.Status.Backup.BackupName != "manual-demo-1" {
		t.Fatalf("backupName = %q", got.Status.Backup.BackupName)
	}

	// Three more reconciles on the same annotation value: the backup is polled,
	// never re-triggered.
	for i := range 3 {
		if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
			t.Fatalf("reconcile %d: %v", i+2, err)
		}
	}
	if trigger, _, _, _ := ops.counts(); trigger != 1 {
		t.Fatalf("TriggerVeleroBackup appelé %d fois, attendu 1", trigger)
	}
	got = fetch(t, ctx, obj)
	if got.Status.Backup.Phase != "Completed" {
		t.Fatalf("phase = %q, attendu Completed", got.Status.Backup.Phase)
	}
	requireCond(t, got, v1alpha1.CondBackupCompleted, metav1.ConditionTrue, "Completed")

	// A *new* value is a new request.
	got.Annotations[v1alpha1.AnnBackupRequestedAt] = "2026-08-06T15:00:00Z"
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("patch annotation: %v", err)
	}
	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile après nouvelle demande: %v", err)
	}
	if trigger, _, _, _ := ops.counts(); trigger != 2 {
		t.Fatalf("TriggerVeleroBackup appelé %d fois après nouvelle demande, attendu 2", trigger)
	}
}

// Property 2 — two restores never run at once. Today nothing stops two calls to
// CreateVeleroRestore from each starting their own scale-down / PVC-delete
// sequence on the same vcluster; the controller closes that.
func TestConcurrentRestoreIsDeferredNotStarted(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{restoreName: "r-second"}
	r := newReconciler(ops)

	obj := newMarker(t, ctx, "restore-busy", map[string]string{
		v1alpha1.AnnRestoreRequestedAt: "2026-08-06T16:00:00Z",
		v1alpha1.AnnRestoreFromBackup:  "manual-demo-1",
	})
	seedRestoreStatus(t, ctx, obj, v1alpha1.RestoreOpsStatus{
		LastHandledRequestedAt: "2026-08-06T15:00:00Z",
		RestoreName:            "r-first",
		Phase:                  "InProgress",
		InPlace:                true,
	})

	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, create, _, _ := ops.counts(); create != 0 {
		t.Fatalf("restore lancé %d fois alors qu'un restore tourne", create)
	}
	got := fetch(t, ctx, obj)
	requireCond(t, got, v1alpha1.CondRestoreRejectedBusy, metav1.ConditionTrue, "AlreadyRunning")
	// The annotation is not consumed: the request is deferred, not lost.
	if got.Status.Restore.LastHandledRequestedAt != "2026-08-06T15:00:00Z" {
		t.Fatalf("la demande concurrente a été consommée: %q", got.Status.Restore.LastHandledRequestedAt)
	}
}

// Property 3 — the one that matters most. The process dies mid-sequence, past
// the point of no return. On restart, a level-triggered reconcile must NOT resume
// Flux: that would let the StatefulSet recreate an empty PVC and hide the loss.
// The decision comes from reading the cluster back, not from a record the dead
// process left behind.
func TestInterruptedRestoreWithVolumeGoneDoesNotResumeFlux(t *testing.T) {
	ctx := context.Background()
	obj := newMarker(t, ctx, "interrupted-volume-gone", map[string]string{
		v1alpha1.AnnRestoreRequestedAt: "2026-08-06T17:00:00Z",
		v1alpha1.AnnRestoreFromBackup:  "manual-demo-1",
	})

	// The kill happens inside the sequence, so nothing is written after the
	// reservation.
	reconcileSurvivingAKill(t, ctx, newReconciler(&fakeOps{killDuringSequence: true}), obj)

	got := fetch(t, ctx, obj)
	if got.Status.Restore.LastHandledRequestedAt != "2026-08-06T17:00:00Z" {
		t.Fatalf("la demande n'a pas été réservée avant l'action: %q", got.Status.Restore.LastHandledRequestedAt)
	}
	if got.Status.Restore.RestoreName != "" {
		t.Fatalf("restoreName = %q, attendu vide", got.Status.Restore.RestoreName)
	}

	// Restart: brand-new reconciler, brand-new ops, zero in-process memory. The
	// cluster says the volume is gone and no restore is running.
	fresh := &fakeOps{inspectView: service.InterruptedRestoreView{VolumeGone: true}}
	if _, err := newReconciler(fresh).Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile après redémarrage: %v", err)
	}

	_, create, inspect, abort := fresh.counts()
	if inspect != 1 {
		t.Fatalf("l'état du cluster n'a pas été relu (%d appels)", inspect)
	}
	if abort != 0 {
		t.Fatalf("Flux repris (%d appels) alors que le volume est supprimé — perte de données masquée", abort)
	}
	if create != 0 {
		t.Fatalf("séquence destructrice relancée toute seule (%d appels)", create)
	}
	got = fetch(t, ctx, obj)
	if !got.Status.Restore.VolumeDestroyed {
		t.Fatal("volumeDestroyed non signalé")
	}
	requireCond(t, got, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "VolumeGoneNoRestore")
}

// Property 4 — the mirror case: interrupted before the point of no return, the
// volume is intact, so the repair is to put the vcluster back. Same restart,
// opposite decision, and again nothing was written down to get there.
func TestInterruptedRestoreWithVolumeIntactResumesFlux(t *testing.T) {
	ctx := context.Background()
	obj := newMarker(t, ctx, "interrupted-volume-intact", map[string]string{
		v1alpha1.AnnRestoreRequestedAt: "2026-08-06T18:00:00Z",
		v1alpha1.AnnRestoreFromBackup:  "manual-demo-1",
	})

	reconcileSurvivingAKill(t, ctx, newReconciler(&fakeOps{killDuringSequence: true}), obj)

	fresh := &fakeOps{inspectView: service.InterruptedRestoreView{VolumeGone: false}}
	if _, err := newReconciler(fresh).Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile après redémarrage: %v", err)
	}

	if _, _, _, abort := fresh.counts(); abort != 1 {
		t.Fatalf("AbortInPlaceRestore appelé %d fois, attendu 1 (volume intact ⇒ reprise de Flux)", abort)
	}
	got := fetch(t, ctx, obj)
	if got.Status.Restore.VolumeDestroyed {
		t.Fatal("volumeDestroyed signalé alors que le volume est intact")
	}
	requireCond(t, got, v1alpha1.CondRestoreNeedsRetry, metav1.ConditionTrue, "InterruptedBeforePointOfNoReturn")
}

// Property 5 — the case a written record gets WRONG. The process died between
// creating the Velero Restore and recording its name. A record would say "past
// the point of no return, no restore" and declare a data loss; the cluster says a
// restore is running. Adopt it, or the vcluster stays suspended forever after a
// restore that actually succeeded.
func TestInterruptedRestoreAdoptsARunningRestore(t *testing.T) {
	ctx := context.Background()
	obj := newMarker(t, ctx, "interrupted-adopt", map[string]string{
		v1alpha1.AnnRestoreRequestedAt: "2026-08-06T18:30:00Z",
		v1alpha1.AnnRestoreFromBackup:  "manual-demo-1",
	})

	reconcileSurvivingAKill(t, ctx, newReconciler(&fakeOps{killDuringSequence: true}), obj)

	// Volume gone AND a restore in flight — the ambiguous case.
	fresh := &fakeOps{inspectView: service.InterruptedRestoreView{
		VolumeGone:         true,
		ActiveRestoreName:  "r-adopted",
		ActiveRestorePhase: "InProgress",
	}}
	res, err := newReconciler(fresh).Reconcile(ctx, reqFor(obj))
	if err != nil {
		t.Fatalf("reconcile après redémarrage: %v", err)
	}
	if res.RequeueAfter != RequeueInterval {
		t.Fatalf("requeue %s, attendu %s : le restore adopté doit être suivi", res.RequeueAfter, RequeueInterval)
	}

	got := fetch(t, ctx, obj)
	if got.Status.Restore.RestoreName != "r-adopted" {
		t.Fatalf("restore non adopté: restoreName = %q", got.Status.Restore.RestoreName)
	}
	if got.Status.Restore.VolumeDestroyed {
		t.Fatal("perte de volume signalée alors qu'un restore est en cours de réussite")
	}
	requireCond(t, got, v1alpha1.CondRestoreInProgress, metav1.ConditionTrue, "Adopted")
}

// Property 6 — the reconcile loop replaces resumeAfterInPlaceRestore: it keeps
// requeueing until Flux is confirmed back, and reports "pending" rather than a
// premature success in between. No goroutine, no dependency on a browser still
// polling.
func TestInPlaceRestoreIsFollowedUntilFluxIsBack(t *testing.T) {
	ctx := context.Background()
	// Velero says Completed straight away; what is not settled yet is whether
	// Flux came back — the distinction the UI used to get wrong.
	ops := &fakeOps{restoreStatus: []service.VeleroRestoreStatusView{
		{Phase: "Completed", ResumePending: true},
		{Phase: "Completed"},
	}}
	r := newReconciler(ops)

	obj := newMarker(t, ctx, "restore-follow", map[string]string{
		v1alpha1.AnnRestoreRequestedAt: "2026-08-06T19:00:00Z",
		v1alpha1.AnnRestoreFromBackup:  "manual-demo-1",
	})
	seedRestoreStatus(t, ctx, obj, v1alpha1.RestoreOpsStatus{
		LastHandledRequestedAt: "2026-08-06T19:00:00Z", // already consumed
		RestoreName:            "r-follow",
		Phase:                  "InProgress",
		InPlace:                true,
	})

	res, err := r.Reconcile(ctx, reqFor(obj))
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if res.RequeueAfter != RequeueInterval {
		t.Fatalf("phase terminale mais reprise Flux non confirmée: requeue %s, attendu %s", res.RequeueAfter, RequeueInterval)
	}
	got := fetch(t, ctx, obj)
	if !got.Status.Restore.ResumePending {
		t.Fatal("resumePending non reporté")
	}
	if got.Status.Restore.FirstTerminalAt == nil {
		t.Fatal("firstTerminalAt non horodaté : la borne de renoncement ne survivrait pas à un redémarrage")
	}
	requireCond(t, got, v1alpha1.CondFluxResumePending, metav1.ConditionTrue, "ResumePending")

	res, err = r.Reconcile(ctx, reqFor(obj))
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("requeue %s après reprise confirmée, attendu 0", res.RequeueAfter)
	}
	got = fetch(t, ctx, obj)
	if got.Status.Restore.ResumePending {
		t.Fatal("resumePending toujours vrai après reprise")
	}
	requireCond(t, got, v1alpha1.CondFluxResumePending, metav1.ConditionFalse, "Resumed")
	requireCond(t, got, v1alpha1.CondRestoreInProgress, metav1.ConditionFalse, "Completed")
}

// Property 7 — a resume that never works must end up reported, not retried in
// silence forever. The goroutine this replaces gave up after 2h; that deadline
// now lives in status, so it survives a restart.
func TestFluxResumeGivesUpAfterTheDeadline(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{restoreStatus: []service.VeleroRestoreStatusView{
		{Phase: "Completed", ResumePending: true},
	}}
	obj := newMarker(t, ctx, "resume-gives-up", map[string]string{
		v1alpha1.AnnRestoreRequestedAt: "2026-08-06T20:00:00Z",
		v1alpha1.AnnRestoreFromBackup:  "manual-demo-1",
	})
	// Terminal since well beyond the budget — as a restart would find it.
	longAgo := metav1.NewTime(time.Now().Add(-2 * v1alpha1.ResumeGiveUpAfter))
	seedRestoreStatus(t, ctx, obj, v1alpha1.RestoreOpsStatus{
		LastHandledRequestedAt: "2026-08-06T20:00:00Z",
		RestoreName:            "r-stuck",
		Phase:                  "Completed",
		InPlace:                true,
		ResumePending:          true,
		FirstTerminalAt:        &longAgo,
	})

	res, err := newReconciler(ops).Reconcile(ctx, reqFor(obj))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("requeue %s : on continue de réessayer indéfiniment au lieu de signaler", res.RequeueAfter)
	}
	got := fetch(t, ctx, obj)
	if !got.Status.Restore.ResumeFailed {
		t.Fatal("resumeFailed non positionné : l'échec durable reste invisible")
	}
	if got.Status.Restore.ResumePending {
		t.Fatal("resumePending toujours vrai après renoncement")
	}
	requireCond(t, got, v1alpha1.CondFluxResumePending, metav1.ConditionTrue, "ResumeGaveUp")
}

// Property 8 — the controller only ever writes through the /status subresource.
// Annotations belong to the requester (the app patches them). This is what makes
// the RBAC split of design §5 real rather than aspirational.
func TestControllerWritesStatusOnly(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{backupName: "manual-demo-2", restoreName: "r-status-only"}
	r := newReconciler(ops)

	annotations := map[string]string{
		v1alpha1.AnnBackupRequestedAt:  "2026-08-06T20:00:00Z",
		v1alpha1.AnnRestoreRequestedAt: "2026-08-06T20:00:00Z",
		v1alpha1.AnnRestoreFromBackup:  "manual-demo-1",
		v1alpha1.AnnRestoreRequestedBy: "gmalfray",
	}
	obj := newMarker(t, ctx, "status-only", maps.Clone(annotations))

	for i := range 3 {
		if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}

	got := fetch(t, ctx, obj)
	if !maps.Equal(got.Annotations, annotations) {
		t.Fatalf("annotations modifiées par le contrôleur: %v", got.Annotations)
	}
	// No spec at all, so generation must never move.
	if got.Generation != 1 {
		t.Fatalf("generation = %d, attendu 1 : le contrôleur a écrit hors du status", got.Generation)
	}
	if trigger, create, _, _ := ops.counts(); trigger != 1 || create != 1 {
		t.Fatalf("trigger=%d create=%d, attendu 1 et 1", trigger, create)
	}
}

// The reconcile logic above is exercised by direct calls, which keeps it
// deterministic. This one checks the other half: that the reconciler actually
// registers with a controller-runtime manager against a real API server.
func TestSetupWithManager(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	r := &VeleroOpsReconciler{Client: mgr.GetClient(), Ops: &fakeOps{}}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}
}
