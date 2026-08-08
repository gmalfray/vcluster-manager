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
//
// Il embarque fakeVClusterOps pour les cinq seams qu'il ne teste pas
// (observation, provisionnement, quotas, intégrations) : aucun test de
// suppression n'y touche, la suppression court-circuite reconcileAll avant eux
// (Reconcile() bifurque sur deletionTimestamp avant d'y arriver). Suspend/Resume
// restent définis ici plutôt qu'hérités : le finalizer ne s'en sert jamais non
// plus, mais le champ Ops du reconciler est de ce type.
type fakeDeletionOps struct {
	fakeVClusterOps

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

	// nsAbsent / nsUnknown : par défaut le namespace existe, ce qui est le cas de
	// tous les tests de suppression écrits avant que la question ne se pose.
	nsAbsent  bool
	nsUnknown bool

	// nsSurvivesDelete garde le namespace en place malgré la suppression : le
	// namespace en Terminating qu'un finalizer tiers retient, ou que la
	// Kustomization Flux du tenant réapplique. C'est le cas que N6 doit rapporter
	// au lieu d'annoncer une destruction qu'il n'a pas constatée.
	nsSurvivesDelete bool
	deleteNSErr      error
}

func (f *fakeDeletionOps) HostNamespaceState(_ context.Context, _, _ string) (bool, bool) {
	f.record("namespace-state")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nsUnknown {
		return false, false
	}
	return !f.nsAbsent, true
}

// DeleteHostNamespace fait au faux cluster ce que l'appel fait au vrai : après
// lui, le namespace n'est plus là. C'est ce changement d'état — et non un drapeau
// « delete a été appelé » — qui permet à l'étape suivante de conclure par
// observation, comme en production.
func (f *fakeDeletionOps) DeleteHostNamespace(context.Context, models.Actor, string, string) error {
	f.record("delete-namespace")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteNSErr != nil {
		return f.deleteNSErr
	}
	if !f.nsSurvivesDelete {
		f.nsAbsent = true
	}
	return nil
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

// Observation, provisionnement et quotas viennent de fakeVClusterOps sans être
// redéfinis : Reconcile() bifurque sur deletionTimestamp avant reconcileAll, donc
// aucun test de ce fichier n'atteint ces trois seams — ils n'existent que pour
// que ce faux compile contre VClusterServiceOps.
var _ VClusterServiceOps = (*fakeDeletionOps)(nil)

// unpairedOps est le cas nominal : Rancher n'a jamais connu ce vcluster, aucune
// protection posée, la destruction passe.
func unpairedOps() *fakeDeletionOps {
	return &fakeDeletionOps{protection: service.ProtectionState{Available: true}}
}

// readyToDestroyOps est unpairedOps avec la sauvegarde déjà terminée : la
// séquence atteint sa DERNIÈRE étape en un seul tour.
//
// Sans ça, le premier reconcile lance la sauvegarde et s'arrête là — un test de
// l'étape finale qui ne ferait qu'un tour mesurerait donc l'étape de sauvegarde,
// et passerait au vert en n'exécutant jamais ce qu'il prétend couvrir.
func readyToDestroyOps() *fakeDeletionOps {
	ops := unpairedOps()
	ops.backup = service.DeletionBackupState{Found: true, Name: "b1", Phase: "Completed", Completed: true}
	return ops
}

// --- ce que les seams du finalizer reçoivent réellement --------------------
//
// Même angle mort que celui fermé pour DeleteHostNamespace/HostNamespaceState
// dans vcluster_namespace_removal_test.go : `name` et `env` sont deux `string`
// voisines dans la même signature — `UnpairForDeletion(ctx, actor, vc.Name,
// r.Cell)`, `TriggerVeleroBackup(ctx, actor, vc.Name, r.Cell)`,
// `SetProtection(ctx, actor, vc.Name, r.Cell, false)`,
// `TeardownVCluster(ctx, actor, vc.Name, r.Cell, opts)` — et rien ne vérifiait
// qu'elles n'étaient pas interverties. Ça compile, et en production ça
// supprimerait le vcluster nommé d'après la cell plutôt que la cell.

// argCall retient (acteur, nom, cell) d'un appel du finalizer vers le service.
type argCall struct {
	actor models.Actor
	name  string
	env   string
}

// argCallRecorder embarque fakeDeletionOps pour capter les arguments des
// quatre seams que TestNamespaceRemovalPassesTheVClusterAndTheCell ne couvrait
// pas. Comme namespaceCallRecorder, il délègue au vrai comportement du faux
// après avoir noté l'appel : l'interversion doit se voir sans changer ce que la
// séquence fait.
type argCallRecorder struct {
	*fakeDeletionOps

	mu       sync.Mutex
	unpair   []argCall
	trigger  []argCall
	protect  []argCall
	teardown []argCall
}

var _ VClusterDeletionOps = (*argCallRecorder)(nil)

func (r *argCallRecorder) UnpairForDeletion(ctx context.Context, actor models.Actor, name, env string) error {
	r.mu.Lock()
	r.unpair = append(r.unpair, argCall{actor, name, env})
	r.mu.Unlock()
	return r.fakeDeletionOps.UnpairForDeletion(ctx, actor, name, env)
}

func (r *argCallRecorder) TriggerVeleroBackup(ctx context.Context, actor models.Actor, name, env string) (service.VeleroBackupCreated, error) {
	r.mu.Lock()
	r.trigger = append(r.trigger, argCall{actor, name, env})
	r.mu.Unlock()
	return r.fakeDeletionOps.TriggerVeleroBackup(ctx, actor, name, env)
}

func (r *argCallRecorder) SetProtection(ctx context.Context, actor models.Actor, name, env string, enabled bool) (service.ProtectionState, error) {
	r.mu.Lock()
	r.protect = append(r.protect, argCall{actor, name, env})
	r.mu.Unlock()
	return r.fakeDeletionOps.SetProtection(ctx, actor, name, env, enabled)
}

func (r *argCallRecorder) TeardownVCluster(ctx context.Context, actor models.Actor, name, env string, opts service.TeardownOptions) ([]string, error) {
	r.mu.Lock()
	r.teardown = append(r.teardown, argCall{actor, name, env})
	r.mu.Unlock()
	return r.fakeDeletionOps.TeardownVCluster(ctx, actor, name, env, opts)
}

// requireArgCall vérifie le dernier appel noté : le bon nom, la bonne cell, et
// l'acteur système — un acteur vide ferait boucler la séquence sur ErrForbidden
// sans qu'aucune condition ne le dise.
func requireArgCall(t *testing.T, calls []argCall, wantName, wantEnv string) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("aucun appel noté")
	}
	got := calls[len(calls)-1]
	if got.name != wantName || got.env != wantEnv {
		t.Fatalf("appel(nom=%q, cell=%q), attendu (%s, %s) : les deux chaînes sont interverties, "+
			"c'est %s (nom de la cell) qui serait ciblé au lieu de %s (le vcluster)",
			got.name, got.env, wantName, wantEnv, got.name, wantName)
	}
	if got.actor != SystemActor {
		t.Fatalf("acteur = %+v, attendu %+v : le service refuse tout ce qui n'est pas admin", got.actor, SystemActor)
	}
}

// Le dépairage Rancher (étape 1 de §4.4) reçoit le vcluster et la cell, pas
// l'inverse.
func TestUnpairForDeletionPassesTheVClusterAndTheCell(t *testing.T) {
	ctx := context.Background()
	ops := &argCallRecorder{fakeDeletionOps: unpairedOps()}
	ops.rancher = service.RancherTeardownState{Enabled: true, StillKnown: true}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "args-unpair", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireArgCall(t, ops.unpair, "args-unpair", "cell1")
}

// La sauvegarde d'avant destruction (étape 2) reçoit le vcluster et la cell,
// pas l'inverse.
func TestTriggerVeleroBackupPassesTheVClusterAndTheCell(t *testing.T) {
	ctx := context.Background()
	ops := &argCallRecorder{fakeDeletionOps: unpairedOps()} // pas de sauvegarde connue : en lance une
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "args-trigger", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireArgCall(t, ops.trigger, "args-trigger", "cell1")
}

// Le retrait de la protection et la destruction (étapes 3 et 4) reçoivent le
// vcluster et la cell, pas l'inverse — les deux dans la même passe.
func TestFinalTeardownPassesTheVClusterAndTheCell(t *testing.T) {
	ctx := context.Background()
	ops := &argCallRecorder{fakeDeletionOps: readyToDestroyOps()}
	ops.protection = service.ProtectionState{Available: true, Protected: true}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "args-teardown", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireArgCall(t, ops.protect, "args-teardown", "cell1")
	requireArgCall(t, ops.teardown, "args-teardown", "cell1")
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

// terminatingVCluster rend un CR en Terminating sans passer par l'API server,
// pour les tests qui appellent la séquence directement.
//
// Le deletionTimestamp n'est pas décoratif : l'étape de sauvegarde s'en sert
// comme borne (« une sauvegarde postérieure à la suppression »), et un objet
// construit à la main sans lui la fait déréférencer nil.
func terminatingVCluster(name string) *v1alpha1.VCluster {
	now := metav1.Now()
	return &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{VClusterFinalizer},
		},
		Status: v1alpha1.VClusterStatus{Deletion: &v1alpha1.DeletionStatus{}},
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
	// newFullOps() et pas unpairedOps() : cet objet n'est pas en suppression, donc
	// Reconcile() enchaîne sur reconcileAll en entier (budget, provisionnement,
	// intégrations, observation) avant de revenir ici — unpairedOps() (fakeDeletionOps
	// nu) ne sait faire que la suppression.
	r := &VClusterReconciler{Client: k8sClient, Ops: newFullOps(), Cell: "cell1", Namespace: "default"}

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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

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

	before := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
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

	after := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
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
	fresh := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
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
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
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
// Les restes que le teardown signale doivent arriver jusqu'à la conclusion, qui
// s'écrit une étape plus loin depuis que le finalizer supprime le namespace.
//
// Le test passe par la séquence entière et pas par l'étape seule : c'est
// justement la traversée d'une étape à l'autre qui peut les perdre.
func TestTeardownWarningsLandInTheStatus(t *testing.T) {
	ctx := context.Background()
	ops := readyToDestroyOps()
	ops.teardownWarnings = []string{"clients OIDC Keycloak pas supprimés : 503"}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := terminatingVCluster("restes")
	done, _, err := r.runDeletionSequence(ctx, ops, vc)
	if err != nil || !done {
		t.Fatalf("séquence: done=%v err=%v — un avertissement ne doit pas bloquer la suppression", done, err)
	}
	if !strings.Contains(vc.Status.Deletion.Message, "Keycloak") {
		t.Fatalf("les restes ne sont pas dans le status : %q", vc.Status.Deletion.Message)
	}
	requireVClusterCond(t, vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "Deleted")
}

// --- la protection se relit avant de détruire, pas seulement avant d'écrire --

// Une protection illisible arrête la séquence AVANT toute destruction.
//
// Le code ne levait pas l'annotation quand la lecture échouait — bon réflexe —
// puis continuait quand même : finalizers Flux retirés, Keycloak, Vault, et
// depuis N6 le namespace lui-même. Le garde-fou était donc contourné par la
// panne qu'il aurait dû faire échouer, avec une conséquence irréversible.
//
// Ce n'était pas dangereux avant N6 : la destruction passait par le prune Flux,
// qu'une annotation restée posée retenait. Depuis que le finalizer supprime
// lui-même, plus rien ne la lit sur ce chemin.
func TestDeletionStopsWhenTheProtectionCannotBeRead(t *testing.T) {
	ctx := context.Background()
	ops := readyToDestroyOps()
	ops.protection = service.ProtectionState{Available: false, Detail: "namespaces \"vcluster-x\" is forbidden"}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "protection-illisible-suppr", false, nil)

	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("le CR a été lâché alors qu'on ne sait pas si son namespace est protégé")
	}
	for _, appel := range []string{"teardown", "delete-namespace", "set-protection"} {
		if n := ops.count(appel); n != 0 {
			t.Fatalf("%s appelé %d fois sur une protection illisible : la séquence a détruit "+
				"sans savoir si le garde-fou était posé (trace: %v)", appel, n, ops.trace())
		}
	}
	if res.RequeueAfter != deletionStepRequeue {
		t.Fatalf("requeue %s, attendu %s : une panne de lecture se répare toute seule, "+
			"encore faut-il repasser", res.RequeueAfter, deletionStepRequeue)
	}
	got := fetchVCluster(t, ctx, vc)
	requireVClusterCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "ProtectionUnknown")
	if c := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondVClusterReady); c != nil &&
		!strings.Contains(c.Message, "forbidden") {
		t.Fatalf("la condition ne dit pas POURQUOI la lecture a échoué : %q", c.Message)
	}
}

// Et le status ne doit pas affirmer une levée de protection qu'il n'a pas
// constatée : `protectionEnabled: false` sur une lecture ratée, c'est écrire
// « ce namespace n'est plus protégé » sans avoir regardé.
func TestUnreadableProtectionDoesNotClearTheStatusFlag(t *testing.T) {
	ctx := context.Background()
	ops := readyToDestroyOps()
	ops.protection = service.ProtectionState{Available: false, Detail: "apiserver injoignable"}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := terminatingVCluster("flag-protection")
	vc.Status.ProtectionEnabled = true

	done, _, err := r.runDeletionSequence(ctx, ops, vc)
	if err != nil || done {
		t.Fatalf("séquence: done=%v err=%v — elle devait s'arrêter sur la lecture ratée", done, err)
	}
	if !vc.Status.ProtectionEnabled {
		t.Fatal("protectionEnabled remis à false sans avoir pu lire l'annotation")
	}
}

// --- N6 : le finalizer supprime le namespace qu'il a créé -------------------

// L'étape Destroying ne supprimait rien : elle retirait les finalizers Flux
// « pour que le namespace puisse être supprimé », puis annonçait « séquence de
// suppression terminée ». La suppression elle-même était le prune d'un commit que
// le finalizer n'écrit ni ne vérifie — un CR pouvait donc disparaître en laissant
// derrière lui le namespace, ses pods et son volume.
func TestFinalizerDeletesTheNamespaceItCreated(t *testing.T) {
	ctx := context.Background()
	ops := readyToDestroyOps()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "ns-supprime", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := ops.count("delete-namespace"); n != 1 {
		t.Fatalf("suppression du namespace demandée %d fois, attendu 1 : "+
			"sans elle, le CR part et le namespace reste (trace: %v)", n, ops.trace())
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatal("le CR n'a pas été lâché alors que le namespace a bien disparu")
	}
}

// La suppression du namespace vient APRÈS la sauvegarde, pas avant. C'est tout
// l'ordre de §4.4 : détruire d'abord et sauvegarder ensuite n'aurait pas de sens,
// et le filet ne servirait plus à rien.
func TestNamespaceIsNotDeletedWhileTheBackupBlocks(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.backup = service.DeletionBackupState{Found: true, Name: "b1", Phase: "PartiallyFailed", Failed: true, StartedAt: time.Now()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "ns-apres-backup", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := ops.count("delete-namespace"); n != 0 {
		t.Fatalf("namespace supprimé %d fois alors que la sauvegarde a échoué : le filet ne sert plus à rien", n)
	}
}

// Un namespace qu'on a demandé à supprimer n'est pas un namespace supprimé : il
// passe en Terminating, et quelque chose peut l'y retenir — un finalizer tiers,
// ou la Kustomization Flux du tenant qui le réapplique. Tant que la disparition
// n'est pas CONSTATÉE, le CR reste.
func TestDeletionWaitsForTheNamespaceToActuallyDisappear(t *testing.T) {
	ctx := context.Background()
	ops := readyToDestroyOps()
	ops.nsSurvivesDelete = true
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "ns-tenace", false, nil)

	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("le CR a été lâché alors que le namespace est toujours là : " +
			"c'est exactement le « terminé » sans preuve que N6 corrige")
	}
	if res.RequeueAfter != deletionStepRequeue {
		t.Fatalf("requeue %s, attendu %s : sans requeue, plus personne ne constate la disparition",
			res.RequeueAfter, deletionStepRequeue)
	}
	got := fetchVCluster(t, ctx, vc)
	requireVClusterCond(t, got, v1alpha1.CondNamespaceRemoved, metav1.ConditionFalse, "NamespaceTerminating")
	if c := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondVClusterReady); c != nil && c.Reason == "Deleted" {
		t.Fatalf("Ready annonce « Deleted » alors que le namespace est encore là : %s", c.Message)
	}
}

// Attendre, mais pas pour toujours. Un namespace qu'un finalizer tiers retient ne
// partira pas parce qu'on insiste ; laisser le CR en Terminating indéfiniment
// n'efface rien et ajoute un objet coincé au problème. On lâche, et on NOMME ce
// qui reste — sinon la trace disparaît avec le CR.
func TestNamespaceRemovalWaitIsBounded(t *testing.T) {
	ctx := context.Background()
	ops := readyToDestroyOps()
	ops.nsSurvivesDelete = true
	ops.teardownWarnings = []string{"backend d'auth Vault pas désactivé : 503"}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "ns-borne", false, nil)

	ageCondition(t, ctx, vc, v1alpha1.CondNamespaceRemoved, metav1.ConditionFalse,
		"NamespaceTerminating", namespaceRemovalGiveUpAfter+time.Minute)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatalf("CR toujours retenu au-delà de %s : un tiers en panne ne doit pas coincer un objet pour toujours",
			namespaceRemovalGiveUpAfter)
	}
}

// Le message de fin doit porter le namespace resté debout ET les restes du
// teardown. C'est la dernière chose écrite avant que l'objet disparaisse : ce qui
// n'y est pas n'est plus lisible nulle part.
func TestGivingUpOnTheNamespaceNamesWhatIsLeft(t *testing.T) {
	ctx := context.Background()
	ops := readyToDestroyOps()
	ops.nsSurvivesDelete = true
	ops.teardownWarnings = []string{"backend d'auth Vault pas désactivé : 503"}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := terminatingVCluster("restes-ns")
	apimeta.SetStatusCondition(&vc.Status.Conditions, metav1.Condition{
		Type: v1alpha1.CondNamespaceRemoved, Status: metav1.ConditionFalse,
		Reason: "NamespaceTerminating", Message: "posée par le test",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-namespaceRemovalGiveUpAfter - time.Minute)),
	})

	done, _, err := r.runDeletionSequence(ctx, ops, vc)
	if err != nil || !done {
		t.Fatalf("séquence: done=%v err=%v — la borne doit laisser passer", done, err)
	}
	requireVClusterCond(t, vc, v1alpha1.CondNamespaceRemoved, metav1.ConditionUnknown, "RemovalUnconfirmed")
	for _, want := range []string{"vcluster-restes-ns", "Vault"} {
		if !strings.Contains(vc.Status.Deletion.Message, want) {
			t.Fatalf("le message de fin ne dit pas %q : %q", want, vc.Status.Deletion.Message)
		}
	}
}

// Un refus de l'API server (RBAC absent, webhook) n'est pas une disparition. Il
// remonte comme erreur, le CR reste, et la condition dit pourquoi.
func TestNamespaceDeletionRefusalKeepsTheCR(t *testing.T) {
	ctx := context.Background()
	ops := readyToDestroyOps()
	ops.deleteNSErr = errors.New("namespaces \"vcluster-ns-refus\" is forbidden")
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "ns-refus", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err == nil {
		t.Fatal("suppression refusée par le cluster mais réconciliation en succès")
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("le CR a été lâché alors que la suppression du namespace a été refusée")
	}
	// Rapportée sur Ready, pas sur NamespaceRemoved : voir le commentaire de
	// reconcileNamespaceRemoval. NamespaceRemoved ne doit être écrite QUE par
	// l'attente, sinon son ancre de délai mesure tantôt un refus tantôt une
	// attente — bug trouvé en recette (cas A), voir namespaceRemovalOverdue et
	// TestARefusalDoesNotLendItsAgeToTheWaitThatFollows plus bas.
	got := fetchVCluster(t, ctx, vc)
	requireVClusterCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "NamespaceDeletionForbidden")
	if c := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondNamespaceRemoved); c != nil {
		t.Fatalf("NamespaceRemoved = %+v : un refus ne doit plus l'écrire du tout", c)
	}
}

// Une vieille condition NamespaceRemoved posée pour une raison étrangère à
// l'attente ne doit pas prêter son âge à l'attente qui commence quand la
// suppression finit par réussir.
//
// C'est le bug trouvé en recette (cas A) : avant le correctif ci-dessus, un
// refus écrivait CondNamespaceRemoved=False/DeleteFailed, et SetStatusCondition
// ne remet LastTransitionTime à zéro que quand le STATUT change, pas la raison
// — donc un refus qui durait plus de dix minutes (le scénario le plus probable
// du chantier : le ClusterRole pas redéployé) prêtait sa vieille horloge à
// l'attente qui commençait tout juste dès que quelqu'un corrigeait le
// ClusterRole. Le CR partait au tout premier tour où la suppression aboutissait
// enfin, sans avoir observé la disparition une seule fois.
//
// Ce test sème directement la condition périmée plutôt que de rejouer un vrai
// refus pendant dix minutes : la condition posée à la main, vieille de plus de
// dix minutes et sous une raison qui n'est PAS l'une des deux que l'attente
// écrit elle-même, simule aussi bien une trace laissée par l'ancien code qu'une
// main qui aurait patché l'objet. namespaceRemovalOverdue doit l'ignorer dans
// les deux cas — c'est la défense en profondeur, en plus de la relocalisation
// vérifiée par TestNamespaceDeletionRefusalKeepsTheCR juste au-dessus.
func TestAnOldForeignReasonOnNamespaceRemovedDoesNotEndTheWaitEarly(t *testing.T) {
	ctx := context.Background()
	ops := readyToDestroyOps()
	// La suppression réussit dès ce tour, mais le namespace n'a pas encore eu le
	// temps de disparaître : l'état normal du tout premier tour qui aboutit.
	ops.nsSurvivesDelete = true
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "ancre-etrangere", false, nil)

	ageCondition(t, ctx, vc, v1alpha1.CondNamespaceRemoved, metav1.ConditionFalse, "DeleteFailed",
		namespaceRemovalGiveUpAfter+time.Hour)

	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if vclusterGone(t, ctx, vc) {
		t.Fatal("CR lâché au premier tour où la suppression a réussi : la vieille condition a " +
			"prêté son âge à l'attente qui commençait tout juste — le bug de recette (cas A)")
	}
	got := fetchVCluster(t, ctx, vc)
	c := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondNamespaceRemoved)
	if c == nil {
		t.Fatal("NamespaceRemoved absente après une demande de suppression réussie")
	}
	if c.Reason != "NamespaceTerminating" {
		t.Fatalf("NamespaceRemoved = %s/%s, attendu False/NamespaceTerminating : l'attente a conclu "+
			"prématurément (RemovalUnconfirmed) sur l'âge d'une condition qui n'est pas la sienne",
			c.Status, c.Reason)
	}
	if res.RequeueAfter != deletionStepRequeue {
		t.Fatalf("requeue %s, attendu %s : l'attente vient de commencer, pas de terminer", res.RequeueAfter, deletionStepRequeue)
	}
}

// « Je n'arrive pas à regarder » n'est pas « il a disparu ». Une lecture ratée
// après la demande ne conclut pas la séquence — même discipline que pour le filet
// de sauvegarde, et ici elle évite d'annoncer une destruction non constatée.
func TestUnreadableNamespaceDoesNotConcludeTheDeletion(t *testing.T) {
	ctx := context.Background()
	// La sauvegarde ne doit pas bloquer avant : on veut atteindre la dernière étape.
	ops := readyToDestroyOps()
	ops.nsUnknown = true
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "ns-illisible-fin", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("le CR a été lâché sur une lecture ratée : rien ne prouve que le namespace est parti")
	}
	requireVClusterCond(t, fetchVCluster(t, ctx, vc), v1alpha1.CondNamespaceRemoved, metav1.ConditionFalse, "NamespaceStateUnknown")
}

// Un objet en Terminating sans notre finalizer ne nous appartient plus :
// Kubernetes finit sans nous et le contrôleur ne doit rien tenter.
func TestDeletionWithoutTheFinalizerDoesNothing(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

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

// Supprimer un vcluster jamais matérialisé n'exige pas l'annotation « détruire
// sans filet ».
//
// Le finalizer est posé sur le chemin vivant, avant le contrôle de budget : un CR
// refusé pour dépassement le porte donc quand même. Sa suppression déclenchait
// l'exigence de sauvegarde Velero d'un namespace qui n'existe pas, et le seul
// déblocage était `backup-override`. Normaliser le geste qui désarme le garde-fou
// de données est bien plus dangereux que le cas qu'il débloque.
func TestDeletingANeverMaterialisedVClusterNeedsNoOverride(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.nsAbsent = true
	// Velero refuserait : personne ne doit l'appeler.
	ops.triggerErr = errors.New("velero pas installé")
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := newDeletingVCluster(t, ctx, "jamais-monte", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatal("bloqué sur l'exigence de sauvegarde alors qu'il n'y a rien à sauvegarder : " +
			"le seul déblocage serait l'annotation « détruire sans filet »")
	}
	if n := ops.count("trigger-backup"); n != 0 {
		t.Fatalf("sauvegarde tentée %d fois sur un namespace inexistant", n)
	}
}

// Une lecture ratée garde le filet. « Je n'arrive pas à regarder » n'est pas
// « il n'y a rien » — c'est la même discipline que partout ailleurs, et ici elle
// protège des données.
func TestAnUnreadableNamespaceStillRequiresTheBackup(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.nsUnknown = true
	ops.triggerErr = errors.New("velero pas installé")
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := newDeletingVCluster(t, ctx, "namespace-illisible", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("détruit sans sauvegarde alors qu'on ne sait pas si des données existent")
	}
	if n := ops.count("trigger-backup"); n == 0 {
		t.Fatal("l'étape de sauvegarde a été sautée sur une lecture ratée")
	}
}
