package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// misplacedMarker drops a marker named after one vcluster into somebody else's
// namespace — the exact object an attacker writes.
func misplacedMarker(t *testing.T, ctx context.Context, name, ns string, annotations map[string]string) *v1alpha1.VClusterVeleroOps {
	t.Helper()
	_ = k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	obj := &v1alpha1.VClusterVeleroOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations},
	}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create marker: %v", err)
	}
	return obj
}

// Scénario A de l'audit : le chemin destructeur. Un marqueur nommé d'après le
// vcluster de la victime, déposé dans un namespace où l'attaquant a le droit
// d'écrire, ne doit RIEN déclencher — l'opérateur agit en SystemActor, donc
// aucune autorisation ne s'interpose plus tard dans la chaîne.
func TestMisplacedMarkerCannotDriveAnotherVClustersRestore(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{}
	r := newReconciler(ops)

	obj := misplacedMarker(t, ctx, "prod-client", "namespace-de-lattaquant", map[string]string{
		v1alpha1.AnnRestoreRequestedAt: "2026-08-07T10:00:00Z",
		v1alpha1.AnnRestoreFromBackup:  "un-backup-quelconque",
	})

	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	_, _, restore, _ := ops.counts()
	if restore != 0 {
		t.Fatalf("StartVeleroRestore appelé %d fois depuis un marqueur mal placé : "+
			"le PVC de prod-client aurait été supprimé", restore)
	}
	got := fetch(t, ctx, obj)
	if got.Status.Restore.LastHandledRequestedAt != "" {
		t.Fatal("la demande a été consommée : le marqueur a été traité alors qu'il devait être ignoré")
	}
	requireCond(t, got, v1alpha1.CondAccepted, metav1.ConditionFalse, "NamespaceMismatch")
}

// Scénario B de l'audit : l'exfiltration. Un marqueur nommé « manager » résout
// vers `vcluster-manager`, le namespace de l'app — un backup Velero y exporterait
// le token GitLab, le secret client Keycloak et JWT_SECRET vers le bucket S3.
// Ce chemin-là n'est pas destructeur, donc rien d'autre ne l'arrête.
func TestMarkerCannotBackUpTheAppsOwnNamespace(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{backupName: "exfiltration"}
	r := newReconciler(ops)

	obj := misplacedMarker(t, ctx, "manager", "namespace-de-lattaquant", map[string]string{
		v1alpha1.AnnBackupRequestedAt: "2026-08-07T10:00:00Z",
	})

	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if trigger, _, _, _ := ops.counts(); trigger != 0 {
		t.Fatalf("TriggerVeleroBackup appelé %d fois : les secrets de l'app seraient partis dans le bucket", trigger)
	}
	requireCond(t, fetch(t, ctx, obj), v1alpha1.CondAccepted, metav1.ConditionFalse, "NamespaceMismatch")
}

// Le pendant positif, sans lequel les deux tests ci-dessus passeraient aussi
// avec un opérateur qui refuse tout.
func TestCorrectlyPlacedMarkerIsStillAccepted(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{backupName: "legitime-1"}
	r := newReconciler(ops)

	obj := newMarker(t, ctx, "bien-place", map[string]string{
		v1alpha1.AnnBackupRequestedAt: "2026-08-07T10:00:00Z",
	})

	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if trigger, _, _, _ := ops.counts(); trigger != 1 {
		t.Fatalf("TriggerVeleroBackup appelé %d fois, attendu 1 : la garde refuse un marqueur légitime", trigger)
	}
}

// Même trou sur le CR : `spec.suspend: true` déposé n'importe où endormirait le
// vcluster homonyme.
func TestMisplacedVClusterCannotSuspendItsNamesake(t *testing.T) {
	ctx := context.Background()
	fake := &fakeVClusterOps{}
	r := &VClusterReconciler{Client: k8sClient, Ops: fake, Cell: "cell1", Namespace: "vcluster-manager"}

	_ = k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ailleurs"}})
	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-client", Namespace: "ailleurs"},
		Spec:       v1alpha1.VClusterSpec{Owner: "attaquant", Suspend: true},
	}
	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, vc) }()

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if s, _ := fake.counts(); s != 0 {
		t.Fatalf("SuspendVCluster appelé %d fois depuis un CR mal placé", s)
	}
	got := fetchVCluster(t, ctx, vc)
	if got.Status.Phase == v1alpha1.VClusterPhaseSuspended {
		t.Fatal("le vcluster a été marqué endormi depuis un CR déposé hors du namespace autorisé")
	}
	var accepted *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == v1alpha1.CondAccepted {
			accepted = &got.Status.Conditions[i]
		}
	}
	if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != "NamespaceMismatch" {
		t.Fatalf("condition Accepted = %+v", accepted)
	}
}

// Le scénario que la garde ratait : `vcluster-` + `manager` EST le namespace de
// l'opérateur, donc les deux règles de placement coïncident sur ce nom-là et le
// marqueur est « bien placé » au sens de la garde. Un backup de ce « vcluster »
// exporte les Secrets de l'app — token GitLab, secret client Keycloak,
// JWT_SECRET — vers le bucket S3.
//
// Le chemin destructeur ne s'arrête aujourd'hui que sur une chance de nommage
// (l'app est un Deployment là où le code cherche un StatefulSet). Une chance
// n'est pas un contrôle, d'où le refus explicite.
func TestMarkerNamedAfterTheOperatorNamespaceIsRefused(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{backupName: "exfiltration"}
	r := newReconciler(ops)

	nom := strings.TrimPrefix(service.OperatorNamespace, "vcluster-")
	obj := misplacedMarker(t, ctx, nom, service.OperatorNamespace, map[string]string{
		v1alpha1.AnnBackupRequestedAt: "2026-08-07T10:00:00Z",
	})

	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if trigger, _, _, _ := ops.counts(); trigger != 0 {
		t.Fatalf("TriggerVeleroBackup appelé %d fois sur le namespace de l'app : "+
			"ses secrets seraient partis dans le bucket S3", trigger)
	}
	requireCond(t, fetch(t, ctx, obj), v1alpha1.CondAccepted, metav1.ConditionFalse, "NamespaceMismatch")
}

// Sur le CR, la défense est à l'admission : la règle CEL de la CRD refuse le
// nom avant que l'objet n'existe. C'est le meilleur endroit — l'opérateur n'a
// même pas à le voir passer.
func TestVClusterNamedAfterTheOperatorNamespaceIsRefusedAtAdmission(t *testing.T) {
	ctx := context.Background()
	nom := strings.TrimPrefix(service.OperatorNamespace, "vcluster-")
	_ = k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: service.OperatorNamespace}})

	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: nom, Namespace: service.OperatorNamespace},
		Spec:       v1alpha1.VClusterSpec{Owner: "attaquant", Suspend: true},
	}
	err := k8sClient.Create(ctx, vc)
	if err == nil {
		_ = k8sClient.Delete(ctx, vc)
		t.Fatalf("un VCluster nommé %q a été admis : reconcileProvisioning appliquerait en SSA "+
			"ForceOwnership dans %q, et le finalizer y retirerait les finalizers Flux de l'app "+
			"elle-même", nom, service.OperatorNamespace)
	}
	if !strings.Contains(err.Error(), "réservé") {
		t.Fatalf("refusé, mais pas par la règle de nom réservé : %v", err)
	}
}

// Et la garde en profondeur, testée sans passer par l'API server : elle doit
// refuser toute seule, sans dépendre du fait que la CRD déployée porte bien la
// règle CEL. Un CR créé avant que la règle n'existe se réconcilie encore.
func TestGuardRefusesTheReservedNameOnItsOwn(t *testing.T) {
	nom := strings.TrimPrefix(service.OperatorNamespace, "vcluster-")

	vc := &v1alpha1.VCluster{ObjectMeta: metav1.ObjectMeta{Name: nom, Namespace: service.OperatorNamespace}}
	if reason := vclusterMisplaced(vc, service.OperatorNamespace); reason == "" {
		t.Fatal("la garde accepte le nom réservé : ses deux règles coïncident sur ce nom, " +
			"le namespace attendu EST celui de l'opérateur")
	}

	ops := &v1alpha1.VClusterVeleroOps{ObjectMeta: metav1.ObjectMeta{Name: nom, Namespace: service.OperatorNamespace}}
	if reason := markerMisplaced(ops); reason == "" {
		t.Fatal("la garde accepte un marqueur nommé d'après le namespace de l'opérateur")
	}
}
