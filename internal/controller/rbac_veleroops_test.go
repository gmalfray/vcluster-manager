package controller

// Ce que le ClusterRole de cmd/veleroops-operator accorde, mesuré en faisant
// tourner VeleroOpsReconciler derrière lui — même dispositif que
// rbac_operator_test.go, harnais dans rbac_probe_test.go.
//
// Ce fichier existe parce que le découpage en deux binaires ne vaut que si
// chacun est prouvé à part : que cmd/veleroops-operator peut faire son
// travail avec SON ClusterRole (les tests "Lets...Run" ci-dessous), et que ni
// lui ni cmd/operator ne peuvent faire le travail de l'autre (les tests
// "StopsAt.../CannotDo..." — le vrai livrable de sécurité de ce chantier).

import (
	"context"
	"testing"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
)

// Le reconcile complet d'une sauvegarde à la demande, avec les seuls droits du
// ClusterRole de cmd/veleroops-operator. Deux tours : le premier déclenche
// (TriggerVeleroBackup, `create backups`), le second poll la phase
// (GetVeleroBackupPhase, `get backups`) — les deux verbes que la séparation a
// otés à cmd/operator, qui n'en avait besoin que du premier.
func TestVeleroOpsRBACLetsABackupReconcileRun(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyVeleroOpsRBAC(t, ctx, admin)
	installProbeCRDs(t, ctx, admin)

	proxyURL, rec := veleroopsAPIProxy(t)
	r := veleroopsReconcilerUnderRBAC(t, proxyURL)

	const nom = "rbac-veleroops-backup"
	obj := newMarker(t, ctx, nom, map[string]string{
		v1alpha1.AnnBackupRequestedAt: "2026-08-08T09:00:00Z",
	})
	t.Cleanup(func() { cleanupMarker(t, admin, obj) })

	// Le Backup se crée dans veleroNamespace ("velero-system"), pas dans le
	// namespace du marqueur — sans lui, la création échoue (namespace absent),
	// TriggerVeleroBackup remonte l'erreur, la phase passe à "Failed" (terminale)
	// et le second reconcile n'atteint jamais le poll qu'il est censé couvrir.
	if err := admin.Apply(ctx, ctrlclient.ApplyConfigurationFromUnstructured(nsObject("velero-system")),
		ctrlclient.FieldOwner("rbac-probe"), ctrlclient.ForceOwnership); err != nil {
		t.Fatalf("namespace velero-system : %v", err)
	}

	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile 1 (déclenchement) : %v", err)
	}
	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile 2 (poll de la phase) : %v", err)
	}

	requireNoForbidden(t, rec, veleroopsRBACFile)
	requireExercised(t, rec, map[string]string{
		"create backups": "TriggerVeleroBackup, la sauvegarde à la demande",
		"get backups":    "GetVeleroBackupPhase, le poll qui suit",
	})
}

// Le reconcile complet d'une restauration in-place, la séquence destructrice :
// suspendre Flux, descendre le vcluster à zéro réplique, supprimer son volume,
// créer le Restore. C'est exactement ce que cmd/operator NE PEUT PLUS faire
// depuis la séparation — voir TestOperatorRBACCannotDoVeleroOpsWork.
func TestVeleroOpsRBACLetsARestoreReconcileRun(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyVeleroOpsRBAC(t, ctx, admin)
	installProbeCRDs(t, ctx, admin)

	const nom = "rbac-veleroops-restore"
	backupName := nom + "-manual"

	proxyURL, rec := veleroopsAPIProxy(t)
	r := veleroopsReconcilerUnderRBAC(t, proxyURL)

	// newMarker crée le namespace vcluster-<nom> ET le marqueur ; seedVClusterWorkloads
	// applique les workloads DANS ce namespace déjà là — dans cet ordre, ou le
	// second se heurte à un namespace déjà créé par le premier.
	obj := newMarker(t, ctx, nom, map[string]string{
		v1alpha1.AnnRestoreRequestedAt: "2026-08-08T10:00:00Z",
		v1alpha1.AnnRestoreFromBackup:  backupName,
	})
	t.Cleanup(func() { cleanupMarker(t, admin, obj) })
	seedVClusterWorkloads(t, ctx, admin, nom)

	// Le précheck d'un restore in-place exige une sauvegarde en phase Completed
	// AVANT de toucher à quoi que ce soit — sans cette fixture, createVeleroRestore
	// s'arrête au premier appel (get backups) et rien du reste de la séquence
	// n'est exercé. Apply plutôt que Create pour le namespace : idempotent si un
	// autre test de ce paquet l'a déjà posé.
	if err := admin.Apply(ctx, ctrlclient.ApplyConfigurationFromUnstructured(nsObject("velero-system")),
		ctrlclient.FieldOwner("rbac-probe"), ctrlclient.ForceOwnership); err != nil {
		t.Fatalf("namespace velero-system : %v", err)
	}
	if err := admin.Apply(ctx, ctrlclient.ApplyConfigurationFromUnstructured(veleroBackupObject("velero-system", backupName, "Completed")),
		ctrlclient.FieldOwner("rbac-probe"), ctrlclient.ForceOwnership); err != nil {
		t.Fatalf("semis du backup %s : %v", backupName, err)
	}

	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile 1 (déclenchement du restore in-place) : %v", err)
	}
	if _, err := r.Reconcile(ctx, reqFor(obj)); err != nil {
		t.Fatalf("reconcile 2 (poll du restore) : %v", err)
	}

	requireNoForbidden(t, rec, veleroopsRBACFile)
	requireExercised(t, rec, map[string]string{
		"get backups":          "précheck de la phase du backup avant un restore in-place",
		"patch helmreleases":   "SetFluxSuspend, suspension avant restore",
		"patch kustomizations": "SetFluxSuspend, suspension avant restore",
		"get statefulsets":     "détection de topologie (etcd embarqué ou externe)",
		"get deployments":      "l'attente que les pods du plan de contrôle soient partis",
		// Suppression, et non plus descente à zéro réplique : scaler laissait
		// l'objet vivant, Velero le restaurait avec `existingResourcePolicy:
		// update` — donc remontait les replicas — et le contrôleur recréait le
		// pod lui-même, sans l'init container `restore-wait`. Le volume revenait
		// vide et la restauration ne finissait jamais.
		"delete deployments":            "suppression du plan de contrôle avant restauration (etcd externe)",
		"delete statefulsets":           "suppression de l'etcd avant restauration",
		"list pods":                     "attente que les pods soient bien partis",
		"delete persistentvolumeclaims": "suppression du volume pour que Velero le recrée",
		"create restores":               "la restauration elle-même",
		"get restores":                  "le poll qui suit, au second tour",
	})
}

// cleanupMarker libère la ressource à la fin d'un test, comme cleanupVCluster
// le fait pour un CR — le marqueur n'a pas de finalizer, une simple Delete
// suffit, mais un test qui plante avant d'y arriver laisserait le namespace
// vcluster-<nom> derrière lui d'un test à l'autre.
func cleanupMarker(t *testing.T, admin ctrlclient.Client, obj *v1alpha1.VClusterVeleroOps) {
	t.Helper()
	_ = admin.Delete(context.Background(), obj)
}

// L'autre moitié, côté veleroops : ce que ce ClusterRole doit REFUSER — tout
// ce qui appartient à cmd/operator (provisionnement, budget, suppression de
// namespace, intégrations externes).
func TestVeleroOpsRBACStopsAtWhatTheDesignRefuses(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyVeleroOpsRBAC(t, ctx, admin)

	for _, cas := range []struct {
		verbe, groupe, ressource, pourquoi string
	}{
		{"create", "vcluster.rebuild-it.fr", "vclusterveleroops",
			"ce reconciler réconcilie les marqueurs que l'app pose, il n'en fabrique pas (séparation des rôles du design §6)"},
		{"patch", "vcluster.rebuild-it.fr", "vclusterveleroops",
			"l'app écrit les annotations du marqueur, ce reconciler uniquement son status"},
		{"delete", "velero.io", "backups",
			"supprimer une sauvegarde est un geste de l'app (cmd/server), pas de ce binaire"},
		{"delete", "", "namespaces",
			"détruire le namespace d'un vcluster est un geste de cmd/operator (arbitrage N6) — ce binaire ne provisionne ni ne détruit aucun vcluster"},
		{"create", "", "configmaps",
			"le ConfigMap de substitutions est provisionné par cmd/operator"},
		{"create", "vcluster.rebuild-it.fr", "vclusters",
			"les VCluster viennent de fluxprod et sont réconciliés par cmd/operator, pas par celui-ci"},
		{"get", "", "resourcequotas",
			"le contrôle de budget appartient à cmd/operator"},
		{"get", "", "secrets",
			"ce binaire ne parle jamais à l'API interne d'un vcluster — pas de kubeconfig à lire"},
		{"create", "", "pods/portforward",
			"même raison : pas d'accès à l'API interne d'un vcluster depuis ce binaire"},
	} {
		t.Run(cas.verbe+" "+cas.ressource, func(t *testing.T) {
			if allowed, raison := subjectMayDo(t, ctx, admin, veleroopsServiceAccount, cas.verbe, cas.groupe, cas.ressource); allowed {
				t.Fatalf("le ClusterRole de cmd/veleroops-operator accorde `%s %s` (%s).\n"+
					"Ce droit est censé lui être refusé : %s", cas.verbe, cas.ressource, raison, cas.pourquoi)
			}
		})
	}
}
