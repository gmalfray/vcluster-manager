package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
)

// fakeDeletionOps tient lieu de cluster, pas de reconciler : c'est lui qu'on
// partage entre deux reconcilers pour simuler un redémarrage, puisque ce qui
// survit à un redémarrage est précisément l'état du cluster.
type fakeDeletionOps struct {
	mu    sync.Mutex
	calls []string

	rancher   service.RancherTeardownState
	unpairErr error

	backup     service.DeletionBackupState
	backupErr  error
	triggerErr error

	protection       service.ProtectionState
	setProtectionErr error

	teardownWarnings []string
	teardownErr      error
}

func (f *fakeDeletionOps) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeDeletionOps) trace() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeDeletionOps) count(call string) int {
	n := 0
	for _, c := range f.trace() {
		if c == call {
			n++
		}
	}
	return n
}

// Les deux méthodes de VClusterOps : le finalizer ne s'en sert pas, mais le
// champ Ops du reconciler est de ce type.
func (f *fakeDeletionOps) SuspendVCluster(context.Context, models.Actor, string, string) error {
	return nil
}
func (f *fakeDeletionOps) ResumeVCluster(context.Context, models.Actor, string, string) error {
	return nil
}

func (f *fakeDeletionOps) InspectRancherTeardown(context.Context, string, string) service.RancherTeardownState {
	f.record("inspect-rancher")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rancher
}

func (f *fakeDeletionOps) UnpairForDeletion(context.Context, models.Actor, string, string) error {
	f.record("unpair")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unpairErr
}

func (f *fakeDeletionOps) InspectDeletionBackup(context.Context, string, string, time.Time) (service.DeletionBackupState, error) {
	f.record("inspect-backup")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backup, f.backupErr
}

func (f *fakeDeletionOps) TriggerVeleroBackup(_ context.Context, _ models.Actor, name, env string) (service.VeleroBackupCreated, error) {
	f.record("trigger-backup")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.triggerErr != nil {
		return service.VeleroBackupCreated{}, f.triggerErr
	}
	// Le cluster connaît désormais cette sauvegarde : c'est ce que la reprise
	// après redémarrage doit pouvoir retrouver toute seule.
	f.backup = service.DeletionBackupState{
		Found: true, Name: "manual-" + name, Phase: "InProgress", StartedAt: time.Now(),
	}
	return service.VeleroBackupCreated{BackupName: f.backup.Name, Name: name, Env: env}, nil
}

func (f *fakeDeletionOps) GetProtection(context.Context, string, string) service.ProtectionState {
	f.record("get-protection")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.protection
}

func (f *fakeDeletionOps) SetProtection(_ context.Context, _ models.Actor, name, env string, enabled bool) (service.ProtectionState, error) {
	f.record("set-protection")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setProtectionErr != nil {
		return service.ProtectionState{}, f.setProtectionErr
	}
	f.protection = service.ProtectionState{Available: true, Protected: enabled, Name: name, Env: env}
	return f.protection, nil
}

func (f *fakeDeletionOps) TeardownVCluster(context.Context, models.Actor, string, string, service.TeardownOptions) ([]string, error) {
	f.record("teardown")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.teardownWarnings, f.teardownErr
}

// unpairedOps est le cas nominal : Rancher n'a jamais connu ce vcluster, aucune
// protection posée, la destruction passe.
func unpairedOps() *fakeDeletionOps {
	return &fakeDeletionOps{protection: service.ProtectionState{Available: true}}
}

// --- outillage -----------------------------------------------------------

// newDeletingVCluster crée un CR avec son finalizer, le supprime, et rend
// l'objet en Terminating tel que le contrôleur le verra.
func newDeletingVCluster(t *testing.T, ctx context.Context, name string, protection bool, annotations map[string]string) *v1alpha1.VCluster {
	t.Helper()
	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Finalizers:  []string{VClusterFinalizer},
			Annotations: annotations,
		},
		Spec: v1alpha1.VClusterSpec{Owner: "greg", DeletionProtection: protection},
	}
	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { releaseVCluster(context.Background(), vc) })

	if err := k8sClient.Delete(ctx, vc); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got := fetchVCluster(t, ctx, vc)
	if got.DeletionTimestamp.IsZero() {
		t.Fatal("pas de deletionTimestamp : le finalizer n'a pas retenu l'objet")
	}
	return got
}

// releaseVCluster débloque un objet resté en Terminating à la fin d'un test —
// sinon il traîne dans l'API server pour toute la suite.
func releaseVCluster(ctx context.Context, vc *v1alpha1.VCluster) {
	var got v1alpha1.VCluster
	key := types.NamespacedName{Name: vc.Name, Namespace: vc.Namespace}
	if err := k8sClient.Get(ctx, key, &got); err != nil {
		return
	}
	got.Finalizers = nil
	_ = k8sClient.Update(ctx, &got)
	_ = k8sClient.Delete(ctx, &got)
}

func vclusterGone(t *testing.T, ctx context.Context, vc *v1alpha1.VCluster) bool {
	t.Helper()
	var got v1alpha1.VCluster
	err := k8sClient.Get(ctx, types.NamespacedName{Name: vc.Name, Namespace: vc.Namespace}, &got)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return false
}

func requireVClusterCond(t *testing.T, vc *v1alpha1.VCluster, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := apimeta.FindStatusCondition(vc.Status.Conditions, condType)
	if c == nil {
		t.Fatalf("condition %s absente (conditions: %+v)", condType, vc.Status.Conditions)
	}
	if c.Status != status {
		t.Fatalf("condition %s: status %s, attendu %s (message: %s)", condType, c.Status, status, c.Message)
	}
	if reason != "" && c.Reason != reason {
		t.Fatalf("condition %s: reason %s, attendu %s (message: %s)", condType, c.Reason, reason, c.Message)
	}
}

// ageCondition réécrit la LastTransitionTime d'une condition pour placer une
// borne dans le passé. C'est le seul moyen de tester un délai de dix minutes ou
// de deux heures sans les attendre.
func ageCondition(t *testing.T, ctx context.Context, vc *v1alpha1.VCluster, condType string, status metav1.ConditionStatus, reason string, age time.Duration) {
	t.Helper()
	got := fetchVCluster(t, ctx, vc)
	if got.Status.Deletion == nil {
		got.Status.Deletion = &v1alpha1.DeletionStatus{}
	}
	apimeta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: "posée par le test",
	})
	c := apimeta.FindStatusCondition(got.Status.Conditions, condType)
	c.LastTransitionTime = metav1.NewTime(time.Now().Add(-age))
	if err := k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("seed condition: %v", err)
	}
}

// --- le finalizer lui-même ------------------------------------------------

// Sans finalizer posé sur le chemin vivant, il n'y a rien pour retenir l'objet
// au moment de la suppression : l'API server refuse d'en ajouter un à un objet
// qui porte déjà un deletionTimestamp.
func TestFinalizerIsPlacedOnTheLivePath(t *testing.T) {
	ctx := context.Background()
	r := &VClusterReconciler{Client: k8sClient, Ops: unpairedOps(), Cell: "cell1"}

	vc := createVCluster(t, ctx, "pose-finalizer", false)
	defer func() { releaseVCluster(ctx, vc) }()

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := fetchVCluster(t, ctx, vc)
	found := false
	for _, f := range got.Finalizers {
		if f == VClusterFinalizer {
			found = true
		}
	}
	if !found {
		t.Fatalf("finalizer absent après réconciliation (finalizers: %v)", got.Finalizers)
	}
}

// Le garde-fou de §4.3 : si le CR est supprimé alors que la protection est
// encore active, la séquence refuse d'avancer, pose une condition et reste
// bloquée. C'est ce qui rend le retrait de la MR acceptable.
func TestDeletionStaysBlockedWhileProtectionIsOn(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}

	vc := newDeletingVCluster(t, ctx, "protege", true, nil)

	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("le vcluster a été supprimé alors que la protection était active")
	}
	if n := ops.count("teardown"); n != 0 {
		t.Fatalf("teardown appelé %d fois malgré la protection", n)
	}
	if n := ops.count("unpair") + ops.count("trigger-backup"); n != 0 {
		t.Fatalf("la séquence a démarré (%d appels) alors qu'elle devait s'arrêter au garde-fou", n)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVClusterCond(t, got, v1alpha1.CondDeletionProtected, metav1.ConditionTrue, "DeletionProtectionBlocked")
	if got.Status.Phase != v1alpha1.VClusterPhaseDeleting {
		t.Fatalf("phase = %q, attendu Deleting", got.Status.Phase)
	}
	if got.Status.Deletion == nil || got.Status.Deletion.Message == "" {
		t.Fatal("aucun message : un blocage muet est exactement ce qu'on veut éviter")
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("requeue %s : le déblocage est un changement de spec, le watch suffit", res.RequeueAfter)
	}
}

// Rejouer le blocage ne doit rien débloquer et rien détruire : un finalizer se
// rejoue à chaque événement sur l'objet.
func TestBlockedDeletionStaysBlockedOnReplay(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}

	vc := newDeletingVCluster(t, ctx, "protege-rejoue", true, nil)
	for i := range 3 {
		if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("supprimé au bout de trois passages malgré la protection")
	}
	if n := ops.count("teardown"); n != 0 {
		t.Fatalf("teardown appelé %d fois", n)
	}
}

// Lever la protection sur un objet déjà en Terminating débloque la séquence :
// c'est la sortie prévue par §4.3.
func TestLiftingProtectionUnblocksTheDeletion(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.backup = service.DeletionBackupState{Found: true, Name: "b1", Phase: "Completed", Completed: true, StartedAt: time.Now()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}

	vc := newDeletingVCluster(t, ctx, "protection-levee", true, nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile bloqué: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("supprimé malgré la protection")
	}

	got := fetchVCluster(t, ctx, vc)
	got.Spec.DeletionProtection = false
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("lever la protection: %v", err)
	}

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile débloqué: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatal("toujours là après la levée de la protection")
	}
	if n := ops.count("teardown"); n != 1 {
		t.Fatalf("teardown appelé %d fois, attendu 1", n)
	}
}

// La séquence nominale, dans l'ordre de §4.4 : dépairage, sauvegarde, retrait
// de la protection, destruction. L'ordre n'est pas cosmétique — le nettoyage
// Rancher tourne DANS le vcluster, et la protection ne doit tomber qu'au
// dernier moment.
func TestDeletionRunsTheStepsInOrder(t *testing.T) {
	ctx := context.Background()
	ops := &fakeDeletionOps{
		rancher:    service.RancherTeardownState{Enabled: true, StillKnown: true},
		protection: service.ProtectionState{Available: true, Protected: true},
	}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}
	vc := newDeletingVCluster(t, ctx, "sequence", false, nil)

	// 1. Rancher connaît encore le cluster : on dépaire et on attend.
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	// 2. Dépairé, nettoyage terminé : la sauvegarde part.
	ops.rancher = service.RancherTeardownState{
		Enabled: true,
		Cleanup: service.CleanupJobState{Observable: true, Found: true, Done: true},
	}
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	// 3. Sauvegarde terminée : protection puis destruction.
	ops.mu.Lock()
	ops.backup.Phase, ops.backup.Completed = "Completed", true
	ops.mu.Unlock()
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}

	if !vclusterGone(t, ctx, vc) {
		t.Fatal("le vcluster n'a pas été supprimé au bout de la séquence")
	}
	want := []string{"unpair", "trigger-backup", "set-protection", "teardown"}
	var got []string
	for _, c := range ops.trace() {
		for _, w := range want {
			if c == w {
				got = append(got, c)
			}
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ordre des étapes = %v, attendu %v (trace complète: %v)", got, want, ops.trace())
	}
}

// La destruction attend que la sauvegarde soit terminée. Détruire pendant
// qu'elle tourne, c'est détruire sans filet en croyant en avoir un.
func TestDeletionWaitsForTheBackupToComplete(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.backup = service.DeletionBackupState{Found: true, Name: "b1", Phase: "InProgress", StartedAt: time.Now()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}
	vc := newDeletingVCluster(t, ctx, "attente-backup", false, nil)

	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("supprimé pendant que la sauvegarde tournait encore")
	}
	if n := ops.count("teardown"); n != 0 {
		t.Fatalf("teardown appelé %d fois avant la fin de la sauvegarde", n)
	}
	if res.RequeueAfter != RequeueInterval {
		t.Fatalf("requeue %s, attendu %s : sans requeue, plus personne ne suit la sauvegarde", res.RequeueAfter, RequeueInterval)
	}
	requireVClusterCond(t, fetchVCluster(t, ctx, vc), v1alpha1.CondVClusterBackupCompleted, metav1.ConditionUnknown, reasonBackupProgress)
}

// Une sauvegarde en échec arrête la séquence et dit ce qu'il faut faire pour
// passer outre. Elle ne laisse pas détruire avec un simple avertissement.
func TestFailedBackupBlocksTheDeletion(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.backup = service.DeletionBackupState{Found: true, Name: "b1", Phase: "PartiallyFailed", Failed: true, StartedAt: time.Now()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}
	vc := newDeletingVCluster(t, ctx, "backup-echec", false, nil)

	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("supprimé alors que la sauvegarde a échoué")
	}
	if n := ops.count("teardown"); n != 0 {
		t.Fatalf("teardown appelé %d fois", n)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("requeue %s : relancer en boucle une sauvegarde en échec ne la répare pas", res.RequeueAfter)
	}
	got := fetchVCluster(t, ctx, vc)
	requireVClusterCond(t, got, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionFalse, "BackupFailed")
	if !strings.Contains(got.Status.Deletion.Message, v1alpha1.AnnDeletionBackupOverride) {
		t.Fatalf("le message ne dit pas comment débloquer : %q", got.Status.Deletion.Message)
	}
}

// L'override est le geste explicite exigé par l'ADR quand il n'y a pas de
// sauvegarde : on force la personne à écrire qu'elle accepte de détruire sans
// filet.
func TestBackupOverrideAnnotationLetsTheDeletionThrough(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.triggerErr = errors.New("velero pas installé")
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}

	vc := newDeletingVCluster(t, ctx, "sans-filet", false, nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("supprimé sans sauvegarde et sans override")
	}
	requireVClusterCond(t, fetchVCluster(t, ctx, vc), v1alpha1.CondVClusterBackupCompleted, metav1.ConditionFalse, "BackupTriggerFailed")

	got := fetchVCluster(t, ctx, vc)
	got.Annotations = map[string]string{v1alpha1.AnnDeletionBackupOverride: "true"}
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("poser l'annotation: %v", err)
	}
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatal("toujours là malgré l'override")
	}
}

// Une sauvegarde qui ne finit jamais doit finir par le dire. Sans borne, le
// vcluster reste en Terminating pour toujours et personne n'est prévenu.
func TestBackupWaitIsBounded(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.backup = service.DeletionBackupState{
		Found: true, Name: "b1", Phase: "InProgress",
		StartedAt: time.Now().Add(-backupGiveUpAfter - time.Hour),
	}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}
	vc := newDeletingVCluster(t, ctx, "backup-sans-fin", false, nil)

	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("requeue %s : on continue de suivre au lieu de renoncer et de le signaler", res.RequeueAfter)
	}
	requireVClusterCond(t, fetchVCluster(t, ctx, vc), v1alpha1.CondVClusterBackupCompleted, metav1.ConditionFalse, "BackupTimedOut")
}

// Rancher injoignable ne doit pas retenir un CR en Terminating éternellement :
// passé la borne, la suppression continue en écrivant ce qui reste à faire à la
// main.
func TestRancherTeardownIsBounded(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.rancher = service.RancherTeardownState{Enabled: true, StillKnown: true, Detail: "état active"}
	ops.backup = service.DeletionBackupState{Found: true, Name: "b1", Phase: "Completed", Completed: true, StartedAt: time.Now()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}

	vc := newDeletingVCluster(t, ctx, "rancher-sans-fin", false, nil)
	ageCondition(t, ctx, vc, v1alpha1.CondRancherPaired, metav1.ConditionTrue, "Unpairing", rancherTeardownGiveUpAfter+time.Minute)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatal("bloqué sur Rancher au-delà de la borne")
	}
	if n := ops.count("unpair"); n != 0 {
		t.Fatalf("dépairage retenté %d fois après la borne", n)
	}
}

// --- reprise après redémarrage -------------------------------------------

// Le cas qui a coûté cher sur le contrôleur voisin : le process meurt entre le
// lancement de la sauvegarde et l'enregistrement de son nom. Un reconciler neuf
// doit retrouver la sauvegarde dans Velero, pas en lancer une deuxième.
func TestRestartAdoptsTheRunningBackupInsteadOfRelaunchingOne(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps() // le faux tient lieu de cluster : il survit au « redémarrage »
	vc := newDeletingVCluster(t, ctx, "reprise-backup", false, nil)

	before := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}
	if _, err := before.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile avant redémarrage: %v", err)
	}
	if n := ops.count("trigger-backup"); n != 1 {
		t.Fatalf("sauvegarde lancée %d fois au premier passage, attendu 1", n)
	}

	// Le status écrit par le process mort est effacé : on ne veut pas que la
	// reprise en dépende. Seul le cluster sait.
	got := fetchVCluster(t, ctx, vc)
	got.Status = v1alpha1.VClusterStatus{}
	if err := k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("effacer le status: %v", err)
	}

	after := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}
	for i := range 3 {
		if _, err := after.Reconcile(ctx, vcReq(vc)); err != nil {
			t.Fatalf("reconcile après redémarrage %d: %v", i+1, err)
		}
	}
	if n := ops.count("trigger-backup"); n != 1 {
		t.Fatalf("sauvegarde lancée %d fois au total : l'opérateur redémarré en a relancé une", n)
	}
	if n := ops.count("teardown"); n != 0 {
		t.Fatalf("détruit pendant que la sauvegarde tournait (%d teardown)", n)
	}
}

// Reprise plus loin dans la séquence : dépairage fait, sauvegarde terminée. Le
// reconciler neuf ne redépaire pas, ne resauvegarde pas, et finit le travail.
func TestRestartMidSequenceResumesWithoutRedoingWhatIsDone(t *testing.T) {
	ctx := context.Background()
	ops := &fakeDeletionOps{
		rancher: service.RancherTeardownState{
			Enabled: true,
			Cleanup: service.CleanupJobState{Observable: true, Found: true, Done: true},
		},
		backup:     service.DeletionBackupState{Found: true, Name: "b1", Phase: "Completed", Completed: true, StartedAt: time.Now()},
		protection: service.ProtectionState{Available: true, Protected: true},
	}
	vc := newDeletingVCluster(t, ctx, "reprise-milieu", false, nil)

	// Status vide : l'opérateur vient de démarrer et n'a rien écrit.
	fresh := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}
	if _, err := fresh.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatalf("séquence pas terminée (trace: %v)", ops.trace())
	}
	if n := ops.count("unpair"); n != 0 {
		t.Fatalf("redépairage alors que Rancher ne connaît plus le vcluster (%d appels)", n)
	}
	if n := ops.count("trigger-backup"); n != 0 {
		t.Fatalf("nouvelle sauvegarde alors qu'une sauvegarde de cette suppression est terminée (%d appels)", n)
	}
	if n := ops.count("teardown"); n != 1 {
		t.Fatalf("teardown appelé %d fois, attendu 1", n)
	}
}

// Une destruction qui échoue laisse l'objet retenu et se rejoue proprement : la
// protection n'est retirée qu'une fois, parce que la deuxième passe constate
// qu'elle est déjà tombée.
func TestFailedTeardownIsRetriedWithoutRedoingTheProtectionRemoval(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.protection = service.ProtectionState{Available: true, Protected: true}
	ops.backup = service.DeletionBackupState{Found: true, Name: "b1", Phase: "Completed", Completed: true, StartedAt: time.Now()}
	ops.teardownErr = errors.New("namespace injoignable")
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}
	vc := newDeletingVCluster(t, ctx, "teardown-echec", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err == nil {
		t.Fatal("l'échec de destruction n'a pas été remonté, donc rien ne sera réessayé")
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("finalizer retiré alors que la destruction a échoué")
	}
	requireVClusterCond(t, fetchVCluster(t, ctx, vc), v1alpha1.CondVClusterReady, metav1.ConditionFalse, "TeardownFailed")

	ops.mu.Lock()
	ops.teardownErr = nil
	ops.mu.Unlock()
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatal("pas supprimé au deuxième essai")
	}
	if n := ops.count("set-protection"); n != 1 {
		t.Fatalf("protection retirée %d fois, attendu 1 : l'étape n'est pas idempotente", n)
	}
	if n := ops.count("teardown"); n != 2 {
		t.Fatalf("teardown appelé %d fois, attendu 2 (un échec puis une réussite)", n)
	}
}

// Ce qui n'a pas pu être nettoyé dehors (Keycloak, Vault, GitLab) ne doit ni
// bloquer la suppression ni disparaître en silence : ça part dans le status,
// qui est la dernière chose lisible avant que l'objet s'en aille.
//
// L'étape est appelée directement : une fois la séquence terminée l'objet
// n'existe plus, donc son dernier status n'est plus relisable par un Get.
func TestTeardownWarningsLandInTheStatus(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.teardownWarnings = []string{"clients OIDC Keycloak pas supprimés : 503"}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}

	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "restes", Namespace: "default"},
		Status:     v1alpha1.VClusterStatus{Deletion: &v1alpha1.DeletionStatus{}},
	}
	done, _, err := r.reconcileFinalTeardown(ctx, ops, vc)
	if err != nil || !done {
		t.Fatalf("étape finale: done=%v err=%v — un avertissement ne doit pas bloquer la suppression", done, err)
	}
	if !strings.Contains(vc.Status.Deletion.Message, "Keycloak") {
		t.Fatalf("les restes ne sont pas dans le status : %q", vc.Status.Deletion.Message)
	}
	requireVClusterCond(t, vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "Deleted")
}

// Un objet en Terminating sans notre finalizer ne nous appartient plus :
// Kubernetes finit sans nous et le contrôleur ne doit rien tenter.
func TestDeletionWithoutTheFinalizerDoesNothing(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1"}

	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sans-finalizer", Namespace: "default",
			Finalizers: []string{"autre.example.com/finalizer"},
		},
		Spec: v1alpha1.VClusterSpec{Owner: "greg"},
	}
	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { releaseVCluster(context.Background(), vc) })
	if err := k8sClient.Delete(ctx, vc); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(ops.trace()) != 0 {
		t.Fatalf("le contrôleur a agi sur un objet qui ne lui appartient pas : %v", ops.trace())
	}
}
