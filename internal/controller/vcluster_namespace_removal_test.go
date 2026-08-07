package controller

import (
	"context"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// --- ce que l'étape passe réellement au service ------------------------------

// nsCall retient les arguments d'un appel du finalizer vers le service.
//
// Les faux du dépôt les jettent tous, et c'est un angle mort : `name` et `env`
// sont deux chaînes voisines dans la même signature, donc les intervertir
// compile sans un mot. En production ça ferait supprimer `vcluster-preprod` —
// le nom de la cell — à la place du vcluster.
type nsCall struct {
	actor models.Actor
	name  string
	env   string
}

type namespaceCallRecorder struct {
	*fakeDeletionOps

	recMu       sync.Mutex
	deleteCalls []nsCall
	stateCalls  []nsCall
}

var _ VClusterDeletionOps = (*namespaceCallRecorder)(nil)

func (r *namespaceCallRecorder) DeleteHostNamespace(ctx context.Context, actor models.Actor, name, env string) error {
	r.recMu.Lock()
	r.deleteCalls = append(r.deleteCalls, nsCall{actor, name, env})
	r.recMu.Unlock()
	return r.fakeDeletionOps.DeleteHostNamespace(ctx, actor, name, env)
}

func (r *namespaceCallRecorder) HostNamespaceState(ctx context.Context, name, env string) (bool, bool) {
	r.recMu.Lock()
	r.stateCalls = append(r.stateCalls, nsCall{name: name, env: env})
	r.recMu.Unlock()
	return r.fakeDeletionOps.HostNamespaceState(ctx, name, env)
}

func (r *namespaceCallRecorder) calls() (del, state []nsCall) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	return append([]nsCall(nil), r.deleteCalls...), append([]nsCall(nil), r.stateCalls...)
}

// Le vcluster et la cell ne sont pas interchangeables, et l'opérateur agit en
// admin.
//
// Les deux se jouent dans un seul appel : `DeleteHostNamespace(ctx, SystemActor,
// vc.Name, r.Cell)`. Passer la cell en premier compile, et le service la
// préfixerait pour supprimer `vcluster-<cell>`. Passer un acteur vide compile
// aussi, et le service rendrait ErrForbidden à chaque tour — une suppression
// qui ne finit jamais, pour une raison qu'aucune condition ne dit.
func TestNamespaceRemovalPassesTheVClusterAndTheCell(t *testing.T) {
	ctx := context.Background()
	ops := &namespaceCallRecorder{fakeDeletionOps: readyToDestroyOps()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}
	vc := newDeletingVCluster(t, ctx, "ns-arguments", false, nil)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	del, state := ops.calls()
	if len(del) != 1 {
		t.Fatalf("%d appel(s) de suppression, attendu 1", len(del))
	}
	if del[0].name != "ns-arguments" || del[0].env != "cell1" {
		t.Fatalf("suppression demandée pour (nom=%q, cell=%q), attendu (ns-arguments, cell1) : "+
			"les deux chaînes sont interverties, c'est vcluster-%s qui part", del[0].name, del[0].env, del[0].name)
	}
	if del[0].actor != SystemActor {
		t.Fatalf("acteur = %+v, attendu %+v : le service refuse tout ce qui n'est pas admin, "+
			"et le finalizer bouclerait sur ErrForbidden", del[0].actor, SystemActor)
	}

	// L'observation qui conclut doit regarder le même objet que la demande.
	// Sinon elle constate la disparition d'un namespace qu'on n'a pas supprimé.
	if len(state) == 0 {
		t.Fatal("aucune lecture du namespace : la séquence conclut sans observer")
	}
	for i, c := range state {
		if c.name != "ns-arguments" || c.env != "cell1" {
			t.Fatalf("lecture %d faite sur (nom=%q, cell=%q) : elle ne porte pas sur le "+
				"namespace qu'on supprime", i, c.name, c.env)
		}
	}
}

// --- l'étape contre un vrai kube-apiserver ----------------------------------

// liveNamespaceOps branche les deux méthodes de namespace sur le cluster de
// test, au lieu du drapeau en mémoire de fakeDeletionOps.
//
// C'est ce qui manquait pour que le chemin complet — provisionnement puis
// suppression — soit mesuré sur l'objet réel. Un faux qui note « delete appelé »
// et bascule un booléen valide la mécanique du finalizer, mais il rendrait
// exactement le même vert si l'étape supprimait le mauvais namespace, ou aucun.
type liveNamespaceOps struct {
	*fullOps
}

var _ VClusterDeletionOps = (*liveNamespaceOps)(nil)

func (o *liveNamespaceOps) DeleteHostNamespace(ctx context.Context, _ models.Actor, name, _ string) error {
	o.record("delete-namespace")
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vcluster-" + name}}
	if err := k8sClient.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (o *liveNamespaceOps) HostNamespaceState(ctx context.Context, name, _ string) (bool, bool) {
	o.record("namespace-state")
	var ns corev1.Namespace
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "vcluster-" + name}, &ns)
	switch {
	case apierrors.IsNotFound(err):
		return false, true
	case err != nil:
		return false, false
	default:
		return true, true
	}
}

func newLiveNamespaceOps() *liveNamespaceOps {
	ops := &liveNamespaceOps{fullOps: newFullOps()}
	ops.backup = readyToDestroyOps().backup
	return ops
}

// Le namespace que l'opérateur a créé au provisionnement est celui qu'il
// supprime à la fin, et c'est le vrai objet qui le prouve.
//
// Sur ce cluster de test il n'y a pas de contrôleur de namespaces : un namespace
// supprimé reste en Terminating pour toujours. Ce n'est pas une gêne, c'est le
// cas de recette qu'on veut — « quelque chose retient le namespace » — et il
// vérifie la moitié qui compte : le CR n'est PAS lâché tant que la disparition
// n'est pas constatée.
func TestTheOperatorDeletesTheHostNamespaceItProvisioned(t *testing.T) {
	ctx := context.Background()
	ops := newLiveNamespaceOps()
	r := budgetedReconciler(ops)

	vc := newProvisioningVCluster(t, ctx, "ns-bout-en-bout", v1alpha1.VClusterSpec{})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("provisionnement: %v", err)
	}
	nsName := types.NamespacedName{Name: "vcluster-ns-bout-en-bout"}
	var ns corev1.Namespace
	if err := k8sClient.Get(ctx, nsName, &ns); err != nil {
		t.Fatalf("préalable : le namespace devait avoir été créé : %v", err)
	}

	if err := k8sClient.Delete(ctx, fetchVCluster(t, ctx, vc)); err != nil {
		t.Fatalf("delete du CR: %v", err)
	}
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile de suppression: %v", err)
	}

	err := k8sClient.Get(ctx, nsName, &ns)
	switch {
	case apierrors.IsNotFound(err):
		// Le namespace est parti pour de bon : le CR doit l'avoir suivi.
		if !vclusterGone(t, ctx, vc) {
			t.Fatalf("le namespace a disparu mais le CR est toujours retenu (trace: %v)", ops.trace())
		}
		t.Log("namespace effectivement supprimé, CR lâché")
	case err != nil:
		t.Fatalf("get namespace: %v", err)
	case ns.DeletionTimestamp.IsZero():
		t.Fatal("le namespace n'a pas de deletionTimestamp : rien ne l'a supprimé. Le CR " +
			"part et le namespace, ses pods et son volume restent sur la cell")
	default:
		// Terminating : demandé, pas encore disparu. C'est ici que se joue N6.
		if vclusterGone(t, ctx, vc) {
			t.Fatal("le CR a été lâché alors que le namespace est encore en Terminating : " +
				"c'est le « terminé » sans preuve que N6 corrige")
		}
		got := fetchVCluster(t, ctx, vc)
		requireVClusterCond(t, got, v1alpha1.CondNamespaceRemoved, metav1.ConditionFalse, "NamespaceTerminating")
		if got.Status.Deletion == nil || got.Status.Deletion.Stage != stageDestroying {
			t.Fatalf("stage = %+v, attendu %s : un humain qui regarde l'objet coincé doit "+
				"voir où il en est", got.Status.Deletion, stageDestroying)
		}
		t.Log("namespace en Terminating, CR retenu : la disparition n'est pas constatée")
	}
}

// Un namespace que rien ne retient s'en va, et le CR avec lui.
//
// Le pendant du test ci-dessus : sans ce cas-là, une étape qui refuserait
// toujours de conclure passerait la moitié « on n'annonce rien sans preuve »
// sans jamais rien terminer. Le namespace n'est pas créé du tout ici, ce qui est
// l'état réel une fois la suppression aboutie.
func TestTheCRIsReleasedOnceTheNamespaceIsReallyGone(t *testing.T) {
	ctx := context.Background()
	ops := newLiveNamespaceOps()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	// Aucun namespace vcluster-ns-deja-parti sur le cluster : l'étape de
	// sauvegarde le constate (rien à sauvegarder) et la dernière étape aussi.
	vc := newDeletingVCluster(t, ctx, "ns-deja-parti", false, nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatalf("CR toujours retenu alors que le namespace n'existe pas (trace: %v)", ops.trace())
	}
	if n := ops.count("delete-namespace"); n != 1 {
		t.Fatalf("suppression demandée %d fois, attendu 1 : l'étape doit demander même "+
			"quand elle croit qu'il n'y a rien — c'est l'observation qui conclut, pas elle", n)
	}
}
