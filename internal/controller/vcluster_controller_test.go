package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/gmalfray/vcluster-manager/internal/models"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
)

// fakeVClusterOps remplace *service.Service pour tout ce qui touche le cluster.
type fakeVClusterOps struct {
	mu           sync.Mutex
	suspendCalls int
	resumeCalls  int
	suspendErr   error
	cellsSeen    []string
}

func (f *fakeVClusterOps) SuspendVCluster(_ context.Context, _ models.Actor, _, env string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspendCalls++
	f.cellsSeen = append(f.cellsSeen, env)
	return f.suspendErr
}

func (f *fakeVClusterOps) ResumeVCluster(_ context.Context, _ models.Actor, _, env string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	f.cellsSeen = append(f.cellsSeen, env)
	return nil
}

func (f *fakeVClusterOps) counts() (suspend, resume int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.suspendCalls, f.resumeCalls
}

func createVCluster(t *testing.T, ctx context.Context, name string, suspend bool) *v1alpha1.VCluster {
	t.Helper()
	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg", Suspend: suspend},
	}
	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("create: %v", err)
	}
	return vc
}

func fetchVCluster(t *testing.T, ctx context.Context, vc *v1alpha1.VCluster) *v1alpha1.VCluster {
	t.Helper()
	var got v1alpha1.VCluster
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: vc.Name, Namespace: vc.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	return &got
}

func vcReq(vc *v1alpha1.VCluster) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: vc.Name, Namespace: vc.Namespace}}
}

// La mise en sommeil ne détruit rien et ouvre une fenêtre d'annulation. C'est ce
// qui rend la suppression réversible, puisque deletionTimestamp ne peut pas
// l'être (crd-vcluster.md §4.2).
func TestSuspendOpensAGracePeriodAndDestroysNothing(t *testing.T) {
	ctx := context.Background()
	ops := &fakeVClusterOps{}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := createVCluster(t, ctx, "sommeil", true)
	defer func() { _ = k8sClient.Delete(ctx, vc) }()

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if s, _ := ops.counts(); s != 1 {
		t.Fatalf("SuspendVCluster appelé %d fois, attendu 1", s)
	}
	got := fetchVCluster(t, ctx, vc)
	if got.Status.Phase != v1alpha1.VClusterPhaseSuspended {
		t.Fatalf("phase = %q, attendu Suspended", got.Status.Phase)
	}
	if got.Status.Deletion == nil || got.Status.Deletion.GracePeriodEndsAt == nil {
		t.Fatal("aucune fenêtre d'annulation ouverte : la suppression ne serait pas réversible")
	}
	if d := time.Until(got.Status.Deletion.GracePeriodEndsAt.Time); d < 6*24*time.Hour {
		t.Fatalf("fenêtre de %s seulement, attendu ~7 jours", d)
	}
}

// Rejouer la réconciliation ne doit pas re-suspendre : l'état vient de la phase,
// donc un opérateur redémarré ne rejoue pas ce qui est déjà fait.
func TestSuspendIsIdempotent(t *testing.T) {
	ctx := context.Background()
	ops := &fakeVClusterOps{}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := createVCluster(t, ctx, "sommeil-idempotent", true)
	defer func() { _ = k8sClient.Delete(ctx, vc) }()

	for i := range 3 {
		if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}
	if s, _ := ops.counts(); s != 1 {
		t.Fatalf("SuspendVCluster appelé %d fois sur 3 réconciliations, attendu 1", s)
	}
}

// L'annulation : repasser suspend à false remet le vcluster debout et referme la
// fenêtre. Laisser la fenêtre ouverte ferait croire à une suppression en cours.
func TestUnsuspendResumesAndClearsTheWindow(t *testing.T) {
	ctx := context.Background()
	ops := &fakeVClusterOps{}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := createVCluster(t, ctx, "annulation", true)
	defer func() { _ = k8sClient.Delete(ctx, vc) }()
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Le revert du commit : suspend repasse à false.
	got := fetchVCluster(t, ctx, vc)
	got.Spec.Suspend = false
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update spec: %v", err)
	}
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	if _, resume := ops.counts(); resume != 1 {
		t.Fatalf("ResumeVCluster appelé %d fois, attendu 1", resume)
	}
	got = fetchVCluster(t, ctx, vc)
	if got.Status.Phase == v1alpha1.VClusterPhaseSuspended {
		t.Fatal("toujours en Suspended après annulation")
	}
	if got.Status.Deletion != nil && got.Status.Deletion.GracePeriodEndsAt != nil {
		t.Fatal("fenêtre d'annulation toujours ouverte : ça se lirait comme une suppression en cours")
	}
}

// Un échec de mise en sommeil ne doit pas laisser croire que c'est fait : la
// phase ne bascule pas, donc la prochaine réconciliation réessaiera.
func TestSuspendFailureDoesNotClaimSuccess(t *testing.T) {
	ctx := context.Background()
	ops := &fakeVClusterOps{suspendErr: errors.New("flux injoignable")}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := createVCluster(t, ctx, "sommeil-echec", true)
	defer func() { _ = k8sClient.Delete(ctx, vc) }()

	if _, err := r.Reconcile(ctx, vcReq(vc)); err == nil {
		t.Fatal("l'échec n'a pas été remonté, donc rien ne sera réessayé")
	}
	got := fetchVCluster(t, ctx, vc)
	if got.Status.Phase == v1alpha1.VClusterPhaseSuspended {
		t.Fatal("phase Suspended alors que la mise en sommeil a échoué")
	}
}

// Le reconciler transmet SA cell, comme celui des marqueurs Velero : l'audit et
// les métriques ne doivent pas annoncer une autre cell que celle où il tourne.
func TestVClusterReconcilerPassesItsCell(t *testing.T) {
	ctx := context.Background()
	ops := &fakeVClusterOps{}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell2", Namespace: "default"}

	vc := createVCluster(t, ctx, "cell-propagation", true)
	defer func() { _ = k8sClient.Delete(ctx, vc) }()
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ops.mu.Lock()
	defer ops.mu.Unlock()
	// Le len() d'abord : sans lui, la boucle ne tourne pas quand le service n'a
	// reçu aucun appel, et le test annonce une propagation qu'il n'a pas vérifiée.
	// C'est le contrôle que son jumeau TestReconcilerPassesItsCellToTheService
	// fait déjà côté marqueurs Velero.
	if len(ops.cellsSeen) == 0 {
		t.Fatal("le service n'a reçu aucune cell : rien n'a été propagé, donc rien n'est vérifié")
	}
	for _, c := range ops.cellsSeen {
		if c != "cell2" {
			t.Fatalf("cell transmise = %q, attendu cell2 (valeurs vues : %v)", c, ops.cellsSeen)
		}
	}
}

// Rejouer une mise en sommeil ne doit pas repousser la fenêtre d'annulation.
//
// Le cas réel : SuspendVCluster réussit, l'écriture du status échoue, et le
// reconcile suivant repasse par ce chemin. Recalculer la date la ferait glisser
// de sept jours à chaque tentative — une écriture qui échoue en boucle donnerait
// une fenêtre qui n'expire jamais, donc une suppression jamais autorisée.
func TestSuspendRetryDoesNotSlideTheGracePeriod(t *testing.T) {
	ops := &fakeVClusterOps{}
	r := &VClusterReconciler{Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newVCluster("rejeu", nil)
	vc.Spec.Suspend = true

	if err := r.reconcileSuspend(context.Background(), vc); err != nil {
		t.Fatalf("premier endormissement : %v", err)
	}
	premiere := vc.Status.Deletion.GracePeriodEndsAt.Time

	// Le status n'a pas pu être écrit : la phase retombe telle qu'elle est en
	// etcd, et le reconcile suivant retente.
	vc.Status.Phase = ""
	if err := r.reconcileSuspend(context.Background(), vc); err != nil {
		t.Fatalf("rejeu : %v", err)
	}

	if got := vc.Status.Deletion.GracePeriodEndsAt.Time; !got.Equal(premiere) {
		t.Fatalf("la fenêtre a glissé de %s au rejeu : elle ne fermerait jamais si l'écriture "+
			"du status échouait en boucle", got.Sub(premiere))
	}
}

// Le pendant : un réveil puis un nouvel endormissement ouvrent bien une
// fenêtre neuve, sinon le correctif ci-dessus figerait une date périmée.
func TestResumeThenSuspendOpensAFreshWindow(t *testing.T) {
	ops := &fakeVClusterOps{}
	r := &VClusterReconciler{Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newVCluster("reveil-rendormi", nil)

	vc.Spec.Suspend = true
	if err := r.reconcileSuspend(context.Background(), vc); err != nil {
		t.Fatalf("endormissement : %v", err)
	}
	ancienne := vc.Status.Deletion.GracePeriodEndsAt.Time

	vc.Spec.Suspend = false
	if err := r.reconcileSuspend(context.Background(), vc); err != nil {
		t.Fatalf("réveil : %v", err)
	}
	if vc.Status.Deletion != nil {
		t.Fatal("la fenêtre survit au réveil : ça se lirait comme une suppression en cours")
	}

	vc.Spec.Suspend = true
	if err := r.reconcileSuspend(context.Background(), vc); err != nil {
		t.Fatalf("second endormissement : %v", err)
	}
	if got := vc.Status.Deletion.GracePeriodEndsAt.Time; got.Equal(ancienne) {
		t.Fatal("la fenêtre du premier sommeil a été réutilisée : elle pourrait être déjà expirée")
	}
}

// Une condition posée sur un chemin d'échec doit atteindre l'API.
//
// C'est l'échec silencieux contre lequel ADR-001 met en garde : si Reconcile
// retourne l'erreur avant d'écrire le status, `kubectl describe` ne montre
// jamais SuspendFailed. L'exploitant voit un reconcile qui boucle et n'a aucun
// moyen de savoir pourquoi — l'objet ment par omission.
func TestAFailurePathStillPublishesItsCondition(t *testing.T) {
	ctx := context.Background()
	ops := &fakeVClusterOps{suspendErr: errors.New("flux injoignable")}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := createVCluster(t, ctx, "condition-sur-echec", true)
	defer func() { _ = k8sClient.Delete(ctx, vc) }()

	if _, err := r.Reconcile(ctx, vcReq(vc)); err == nil {
		t.Fatal("l'échec n'a pas été remonté")
	}

	got := fetchVCluster(t, ctx, vc)
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == v1alpha1.CondVClusterReady {
			ready = &got.Status.Conditions[i]
		}
	}
	if ready == nil {
		t.Fatal("aucune condition publiée après l'échec : kubectl describe ne dirait rien")
	}
	if ready.Reason != "SuspendFailed" {
		t.Fatalf("reason = %q, attendu SuspendFailed", ready.Reason)
	}
	if !strings.Contains(ready.Message, "flux injoignable") {
		t.Fatalf("le message ne porte pas la cause réelle : %q", ready.Message)
	}
}
