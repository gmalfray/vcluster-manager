package controller

// Ce que le ClusterRole de l'opérateur accorde, mesuré en faisant tourner
// l'opérateur derrière lui.
//
// Le harnais est dans rbac_probe_test.go. Ici, les scénarios : on exécute du
// vrai code — le reconcile complet, puis la séquence de suppression — sous
// l'identité du ServiceAccount, et on regarde si l'apiserver a refusé quoi que
// ce soit.

import (
	"context"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
)

// Le canari : sans lui, tout ce fichier pourrait passer au vert en n'appliquant
// aucun RBAC du tout.
//
// C'est le mode de panne le plus probable de ce dispositif, et il est silencieux
// — un en-tête d'impersonation qui ne part pas, un proxy court-circuité, une
// version d'envtest qui démarre l'apiserver en AlwaysAllow : dans les trois cas
// les appels réussissent tous et « aucun 403 » ne prouve plus rien. Exactement
// le faux vert que ce chantier vient fermer, un cran plus haut.
//
// On demande donc une ressource que le ClusterRole n'accorde nulle part et on
// EXIGE le refus. Si ce test tombe, les autres de ce fichier ne mesurent rien.
func TestTheRBACProbeIsActuallyEnforcing(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyOperatorRBAC(t, ctx, admin)

	proxyURL, rec := operatorAPIProxy(t)
	dyn, err := dynamic.NewForConfig(&rest.Config{Host: proxyURL})
	if err != nil {
		t.Fatalf("client dynamique : %v", err)
	}

	// Des Nodes : rien dans deploy/base/operator-rbac.yaml n'en accorde, et
	// c'est une ressource intégrée, donc l'apiserver la connaît — un 404
	// signifierait autre chose qu'un refus.
	_, err = dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}).
		List(ctx, metav1.ListOptions{})
	if err == nil {
		t.Fatal("`list nodes` a réussi sous l'identité du ServiceAccount de l'opérateur. " +
			"Le ClusterRole ne l'accorde pas : l'autorisation n'est donc pas appliquée, et " +
			"tous les autres tests de ce fichier passent au vert sans rien vérifier.")
	}
	if len(rec.forbidden()) == 0 {
		t.Fatalf("`list nodes` a échoué (%v) mais aucun 403 n'a été enregistré : "+
			"l'enregistreur ne voit pas les refus, donc requireNoForbidden ne peut plus rien attraper", err)
	}

	// Deuxième moitié du canari : le refus doit être ciblé, pas général. Une
	// impersonation vers un sujet inexistant refuserait TOUT, y compris ce que le
	// ClusterRole accorde — et « aucun test ne passe » se remarque, mais pas
	// forcément pour la bonne raison.
	if _, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}).
		Namespace("").List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatalf("`list resourcequotas` refusé à %s : %v\n"+
			"Deux causes possibles, et elles ne se corrigent pas au même endroit :\n"+
			"  - le ClusterRole ne porte plus ce verbe → c'est le bug du 2026-08-08, à corriger dans %s ;\n"+
			"  - le ClusterRoleBinding ne vise plus ce ServiceAccount → l'opérateur n'a alors AUCUN droit, "+
			"et tous les autres tests de ce fichier tombent avec celui-ci.",
			operatorServiceAccount, err, operatorRBACFile)
	}
}

// Le reconcile complet d'un VCluster, du premier passage jusqu'au status écrit,
// avec les seuls droits du ClusterRole commité.
//
// C'est le test qui aurait attrapé l'incident du 2026-08-08 : `list
// resourcequotas` manquait, l'étape de budget est la troisième du reconcile, et
// elle rend une erreur — donc plus rien n'était provisionné. Ici cette étape
// n'est pas simulée : BudgetOps est le VRAI service, branché sur un vrai client
// Kubernetes, comme dans cmd/operator/main.go.
func TestOperatorRBACLetsAFullVClusterReconcileRun(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyOperatorRBAC(t, ctx, admin)

	proxyURL, rec := operatorAPIProxy(t)
	r, _ := operatorReconcilerUnderRBAC(t, proxyURL)

	const nom = "rbac-cycle"
	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: nom, Namespace: DefaultVClustersNamespace},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg"},
	}
	if err := admin.Create(ctx, vc); err != nil {
		t.Fatalf("création du CR : %v", err)
	}
	t.Cleanup(func() { cleanupVCluster(t, admin, nom) })

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile refusé sous le ClusterRole de l'opérateur : %v", err)
	}

	requireNoForbidden(t, rec, operatorRBACFile)

	// Plancher de couverture. Sans lui, un refactor qui déplacerait une étape
	// hors du reconcile ferait passer ce test au vert en n'exerçant plus le droit
	// qu'il est censé garder — le même genre de vert que celui qu'on ferme.
	requireExercised(t, rec, map[string]string{
		"get vclusters":           "la lecture du CR au début du reconcile",
		"update vclusters":        "la pose du finalizer, seule écriture hors sous-ressource",
		"update vclusters/status": "l'écriture du status, faite sur tous les chemins",
		"list resourcequotas":     "l'étape de budget — c'est le droit qui manquait le 2026-08-08",
		"patch namespaces":        "le Server-Side Apply du namespace du tenant",
		"patch configmaps":        "le Server-Side Apply du ConfigMap de substitutions",
	})
}

// La mise en sommeil, `spec.suspend`, avec les seuls droits du ClusterRole
// commité.
//
// C'est l'étape qui a fait ajouter `patch` sur helmreleases/kustomizations/
// statefulsets/deployments à ce ClusterRole : SuspendVCluster
// (internal/service/vcluster_lifecycle.go) suspend Flux ET scale le vcluster
// à zéro réplique — EXACTEMENT les deux appels que fait aussi une
// restauration in-place, pour une raison différente. Avant ce test, ces
// droits n'étaient exercés sous RBAC que côté cmd/veleroops-operator ; rien
// ne prouvait que cmd/operator en avait besoin lui aussi.
func TestOperatorRBACLetsSuspendReconcileRun(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyOperatorRBAC(t, ctx, admin)
	installProbeCRDs(t, ctx, admin)

	proxyURL, rec := operatorAPIProxy(t)
	r, _ := operatorReconcilerUnderRBAC(t, proxyURL)

	const nom = "rbac-suspend"
	seedVClusterWorkloads(t, ctx, admin, nom)

	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: nom, Namespace: DefaultVClustersNamespace},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg", Suspend: true},
	}
	if err := admin.Create(ctx, vc); err != nil {
		t.Fatalf("création du CR : %v", err)
	}
	t.Cleanup(func() { cleanupVCluster(t, admin, nom) })

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile de mise en sommeil refusé sous le ClusterRole de l'opérateur : %v", err)
	}

	requireNoForbidden(t, rec, operatorRBACFile)
	requireExercised(t, rec, map[string]string{
		"patch helmreleases":   "SuspendVCluster suspend Flux avant de descendre les répliques",
		"patch kustomizations": "même chose, sur la Kustomization du tenant",
		"get statefulsets":     "détection de topologie (etcd embarqué ou externe)",
		"patch deployments":    "descente à zéro réplique du plan de contrôle (etcd externe)",
		"patch statefulsets":   "descente à zéro réplique de l'etcd",
	})
}

// La séquence de suppression, sous les mêmes droits.
//
// Elle touche ce que le chemin vivant ne touche pas : lire et retirer
// l'annotation de protection du namespace, puis supprimer le namespace lui-même.
// Ce dernier droit est arrivé tard (arbitrage N6) et le code le sait — il a une
// branche « suppression du namespace refusée : vérifier que le ClusterRole porte
// bien `delete` sur les namespaces ». Écrire cette branche ne garantit pas que
// le droit soit là ; ce test le garantit.
func TestOperatorRBACLetsTheDeletionSequenceRun(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyOperatorRBAC(t, ctx, admin)

	proxyURL, rec := operatorAPIProxy(t)
	r, svc := operatorReconcilerUnderRBAC(t, proxyURL)

	const nom = "rbac-suppression"
	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: nom, Namespace: DefaultVClustersNamespace},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg"},
	}
	if err := admin.Create(ctx, vc); err != nil {
		t.Fatalf("création du CR : %v", err)
	}
	t.Cleanup(func() { cleanupVCluster(t, admin, nom) })

	// Un tour vivant d'abord : il pose le finalizer et matérialise le namespace,
	// sans quoi la séquence de suppression n'aurait rien à supprimer et sauterait
	// justement les étapes qu'on veut voir.
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile initial : %v", err)
	}

	// Poser la protection sous l'identité de l'opérateur : c'est le `patch`
	// namespace que la séquence devra ensuite refaire pour la retirer. Sans ce
	// geste le namespace n'est pas protégé, la branche de retrait n'est jamais
	// prise, et le droit d'écriture sur les namespaces reste non exercé.
	if _, err := svc.SetProtection(ctx, SystemActor, nom, "preprod", true); err != nil {
		t.Fatalf("pose de la protection sous le ClusterRole de l'opérateur : %v", err)
	}

	if err := admin.Delete(ctx, vc); err != nil {
		t.Fatalf("suppression du CR : %v", err)
	}
	// Deux tours : la séquence rend la main entre les étapes dès qu'une d'elles
	// demande un requeue, et c'est le second qui atteint la suppression du
	// namespace sur certains enchaînements.
	for i := range 2 {
		if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
			t.Fatalf("reconcile de suppression, tour %d : %v", i+1, err)
		}
	}

	requireNoForbidden(t, rec, operatorRBACFile)

	requireExercised(t, rec, map[string]string{
		"get namespaces": "la lecture de l'état du namespace hôte, qui conclut la séquence",
		// `update` et pas `patch` : SetNamespaceProtection relit le namespace puis
		// le réécrit entier. Le `patch` sur les namespaces vient d'ailleurs (le
		// Server-Side Apply du provisionnement), donc l'exiger ici ne prouverait
		// pas que la protection a été touchée.
		"update namespaces": "la pose puis le retrait de l'annotation protect-deletion",
		"delete namespaces": "la dernière étape du finalizer (arbitrage N6)",
	})
}

// Le second ClusterRole du dépôt : celui de l'app (cmd/server).
//
// Il existe parce que les deux ne couvrent PAS les mêmes appels — c'est
// exactement le constat de l'incident : « c'est en dérivant celui de l'opérateur
// de ce que le chemin backup/restore touche qu'on a perdu ce que touche le
// chemin d'admission ». L'app pose les ordres Velero et supprime les
// sauvegardes ; l'opérateur les exécute et ne supprime rien.
//
// Ce que ce test ne couvre pas, et il faut le dire : l'app ne se réduit pas à
// ces appels-là. Tout ce qui passe par un port-forward vers l'API interne d'un
// vcluster (kubeconfig, apps ArgoCD, webhook Vault, jobs) n'est pas joignable
// hors ligne, et GetVClusterStatus en dépend. Le périmètre couvert ici est le
// client dynamique sur le cluster HÔTE.
func TestAppRBACCoversTheClusterCallsTheServerMakes(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyAppRBAC(t, ctx, admin)
	installProbeCRDs(t, ctx, admin)

	proxyURL, rec := apiProxyAs(t, appServiceAccount)
	k8s := operatorStatusClient(t, proxyURL)

	const nom = "rbac-app"
	seedVClusterWorkloads(t, ctx, admin, nom)

	// Ce que l'app fait et que l'opérateur ne fait pas.
	_, _ = k8s.ListVeleroBackups(ctx, nom, "velero-system")
	_, _ = k8s.CreateVeleroBackup(ctx, nom, "velero-system", "720h0m0s", "")
	_ = k8s.DeleteVeleroBackup(ctx, nom+"-manual", "velero-system")
	_ = k8s.RequestVeleroOps(ctx, nom, map[string]string{"vcluster.rebuild-it.fr/backup-requested-at": "1"})
	_, _ = k8s.GetVeleroOpsRestoreState(ctx, nom)
	// Borné court : GetBackupContentURL attend qu'un DownloadRequest passe en
	// Processed pendant 30 s, et Velero n'est pas là pour le traiter. Ce qui nous
	// intéresse — la création, la lecture, puis la suppression du DownloadRequest —
	// est déjà parti avant la première attente.
	contentCtx, annule := context.WithTimeout(ctx, 2*time.Second)
	_, _ = k8s.GetBackupContentURL(contentCtx, nom+"-manual", "velero-system")
	annule()

	// Et ce qu'elle partage avec l'opérateur, sur les mêmes objets mais avec un
	// autre ClusterRole : rien ne garantit que les deux listes coïncident.
	_, _ = k8s.CreateVeleroRestore(ctx, nom+"-manual", "vcluster-"+nom, "vcluster-"+nom, "velero-system")
	_, _ = k8s.ListActiveVeleroRestores(ctx, nom, "velero-system")
	_ = k8s.SetFluxSuspend(ctx, nom, true)
	_ = k8s.CleanupNamespace(ctx, nom)
	_ = k8s.ScaleVClusterWorkloads(ctx, nom, 0)
	_, _, _ = k8s.GetVClusterPVCState(ctx, nom)
	_ = k8s.DeleteVClusterPVC(ctx, nom)
	_, _ = k8s.CountVClusterPods(ctx, nom)
	_, _ = k8s.GetNamespaceProtection(ctx, nom)
	_ = k8s.SetNamespaceProtection(ctx, nom, true)
	_, _ = k8s.SumVClusterQuotaHardLimits(ctx, "")
	_, _ = k8s.GetKubeconfig(ctx, nom)

	requireNoForbidden(t, rec, appRBACFile)

	requireExercised(t, rec, map[string]string{
		// `velero backup delete` ne supprime pas l'objet : il crée un
		// DeleteBackupRequest, que Velero traite pour purger le bucket PUIS
		// retirer le Backup. Un `delete` direct laissait les données dans le
		// bucket et l'objet revenait par la synchronisation.
		"create deletebackuprequests": "la suppression d'une sauvegarde depuis l'UI",
		"create downloadrequests":     "le contenu d'un backup",
		"patch vclusterveleroops":     "l'ordre de backup/restore que l'opérateur réconcilie",
		"update namespaces":           "le bouton de protection de namespace",
		"update kustomizations":       "le retrait des finalizers Flux au nettoyage",
		"get secrets":                 "le kubeconfig qu'on télécharge depuis l'UI",
		"list resourcequotas":         "l'usage des quotas affiché sur le dashboard",
	})
}

// Les appels que le service émet HORS du reconcile — Velero, Flux, le
// comptage de pods, le secret de kubeconfig — pour ce que cmd/operator
// (VCluster) touche RÉELLEMENT. Avant la séparation en deux binaires, ce test
// couvrait aussi Velero restores, le scale des workloads et la suppression de
// volume : ces trois-là ont migré vers rbac_veleroops_test.go, avec le
// ClusterRole qui va avec — VClusterServiceOps ne les appelle jamais (voir
// vcluster_finalizer.go, vcluster_status.go).
//
// Ils ne passent pas par le client controller-runtime mais par le client
// dynamique de internal/kubernetes, et le reconcile ne les déclenche pas sur le
// chemin nominal : c'est exactement le périmètre où le trou `patch helmreleases`
// du ClusterRole de l'app avait été trouvé, en recette, un mois plus tôt.
//
// Ce qui est maintenu à la main ici, ce n'est pas une liste de verbes — c'est la
// liste des OPÉRATIONS que l'opérateur sait faire. Si l'une d'elles change ce
// qu'elle touche, le test suit sans qu'on y retouche.
//
// Les CRD Velero et Flux ne sont pas installées dans envtest : ces appels
// répondent 404. C'est sans importance et c'est même le point — l'autorisation
// est décidée avant que l'apiserver ne cherche la ressource, donc un droit
// manquant rend 403 même sur une CRD absente. Le canari du haut de ce fichier
// vérifie que cette distinction tient.
func TestOperatorRBACCoversTheCallsTheServiceMakesOutsideTheReconcile(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyOperatorRBAC(t, ctx, admin)

	installProbeCRDs(t, ctx, admin)

	proxyURL, rec := operatorAPIProxy(t)
	k8s := operatorStatusClient(t, proxyURL)

	const nom = "rbac-service"
	seedVClusterWorkloads(t, ctx, admin, nom)

	// Le résultat métier de ces appels n'a aucune importance ici, et plusieurs
	// échoueront (objet absent, contenu incomplet). On ne regarde que le code
	// retour HTTP, via le proxy — c'est ce qui permet de couvrir aussi les
	// fonctions qui avalent leur erreur (HostNamespaceState, CountVClusterPods)
	// et dont un refus deviendrait un simple « je ne sais pas ».
	_, _ = k8s.ListVeleroBackups(ctx, nom, "velero-system")                  // InspectDeletionBackup
	_, _ = k8s.CreateVeleroBackup(ctx, nom, "velero-system", "720h0m0s", "") // TriggerVeleroBackup
	_, _ = k8s.GetVClusterStatus(ctx, nom)                                   // status affiché : HelmRelease + Kustomization du tenant
	_ = k8s.CleanupNamespace(ctx, nom)                                       // TeardownVCluster, retrait des finalizers Flux
	_, _ = k8s.CountVClusterPods(ctx, nom)
	_, _ = k8s.HostNamespaceState(ctx, nom)
	_, _ = k8s.GetNamespaceProtection(ctx, nom)
	_, _ = k8s.SumVClusterQuotaHardLimits(ctx, "")
	// Le kubeconfig du vcluster vit dans un Secret du namespace hôte. C'est la
	// PORTE d'entrée vers l'API interne — ce qui vient après (le port-forward
	// SPDY vers le pod du plan de contrôle) n'est pas joignable hors ligne, et
	// reste donc non couvert : voir le commentaire de tête de rbac_probe_test.go.
	_, _ = k8s.GetKubeconfig(ctx, nom)

	// Volontairement ABSENTS de cette liste, et c'est le point du découpage :
	// GetVeleroBackupPhase, CreateVeleroRestore, GetRestoreStatus,
	// ListActiveVeleroRestores, SetFluxSuspend, ScaleVClusterWorkloads,
	// GetVClusterPVCState, DeleteVClusterPVC — aucun de ces appels n'est
	// atteignable depuis VClusterServiceOps ; ils sont couverts, avec le
	// ClusterRole de cmd/veleroops-operator, par rbac_veleroops_test.go.
	// GetVeleroOpsRestoreState aussi : c'est cmd/server qui lit le status que
	// l'opérateur écrit, pas l'opérateur lui-même.

	requireNoForbidden(t, rec, operatorRBACFile)

	requireExercised(t, rec, map[string]string{
		"list backups":          "InspectDeletionBackup, la recherche de la sauvegarde qui couvre une suppression",
		"create backups":        "TriggerVeleroBackup, la sauvegarde d'avant destruction",
		"get helmreleases":      "le status affiché (GetVClusterStatus)",
		"get kustomizations":    "même chose, sur la Kustomization du tenant",
		"list helmreleases":     "CleanupNamespace, avant de retirer les finalizers Flux",
		"list kustomizations":   "même chose",
		"update helmreleases":   "le retrait des finalizers Flux, sans quoi le namespace reste en Terminating",
		"update kustomizations": "même chose, sur la Kustomization <nom>-apps du tenant",
		"list pods":             "le comptage des pods du vcluster",
		"get namespaces":        "l'état du namespace hôte",
		"get secrets":           "le kubeconfig interne du vcluster",
		"list resourcequotas":   "l'addition des quotas de la cell",
	})
}

// L'autre moitié : ce que le ClusterRole doit REFUSER.
//
// deploy/base/operator-rbac.yaml documente plusieurs droits volontairement
// absents — pas de `create` sur les marqueurs Velero (« l'opérateur ne fabrique
// pas de marqueur, il réconcilie ceux que l'app pose »), pas de `delete` sur les
// sauvegardes Velero (« appelé par l'app et non par l'opérateur — retiré ici
// volontairement »), pas de `create` sur les vclusters. Ces phrases sont des
// décisions de conception ; tant qu'elles ne sont que des commentaires, un
// `verbs: ["*"]` posé un jour de dépannage ne fait tomber personne.
//
// Passe par SubjectAccessReview plutôt que par de vrais appels : la question est
// « aurait-il le droit », pas « que se passe-t-il s'il essaie », et un test qui
// vérifierait `create vclusters` en créant un CR le créerait pour de bon si la
// réponse est mauvaise.
func TestOperatorRBACStopsAtWhatTheDesignRefuses(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyOperatorRBAC(t, ctx, admin)

	for _, cas := range []struct {
		verbe, groupe, ressource, pourquoi string
	}{
		{"create", "vcluster.rebuild-it.fr", "vclusterveleroops",
			"l'opérateur réconcilie les marqueurs que l'app pose, il n'en fabrique pas (séparation des rôles du design §6)"},
		{"patch", "vcluster.rebuild-it.fr", "vclusterveleroops",
			"l'app écrit les annotations du marqueur, l'opérateur uniquement son status"},
		{"create", "vcluster.rebuild-it.fr", "vclusters",
			"les VCluster viennent de fluxprod ; qui peut en créer un peut faire détruire le namespace du même nom"},
		{"delete", "velero.io", "backups",
			"supprimer une sauvegarde est un geste de l'app, pas de l'opérateur — c'est le filet de la suppression"},
		{"delete", "", "secrets",
			"l'opérateur ne lit qu'un kubeconfig interne, il n'a rien à écrire ni à effacer dans les secrets"},
		{"list", "", "nodes",
			"rien dans le chemin de l'opérateur ne regarde les nœuds"},
	} {
		t.Run(cas.verbe+" "+cas.ressource, func(t *testing.T) {
			if allowed, raison := subjectMayDo(t, ctx, admin, operatorServiceAccount, cas.verbe, cas.groupe, cas.ressource); allowed {
				t.Fatalf("le ClusterRole accorde `%s %s` au ServiceAccount de l'opérateur (%s).\n"+
					"Ce droit est censé lui être refusé : %s", cas.verbe, cas.ressource, raison, cas.pourquoi)
			}
		})
	}
}

// Le vrai livrable de sécurité de ce découpage : prouver que le ServiceAccount
// de cmd/operator ne peut PLUS faire ce que cmd/veleroops-operator fait de
// PROPRE à lui — piloter un Restore Velero, supprimer un volume. Avant la
// séparation, les deux tournaient dans le même pod sous le même ClusterRole,
// donc chacun de ces refus aurait échoué.
//
// Ce que ce test ne dit PAS refusé, et c'est volontaire : `patch` sur
// helmreleases/kustomizations/statefulsets/deployments. `spec.suspend`
// (TestOperatorRBACLetsSuspendReconcileRun) suspend Flux et scale le vcluster
// exactement comme le fait une restauration in-place — les deux binaires ont
// chacun leur propre raison d'y toucher, et prétendre le contraire ici serait
// un test qui ment. Ce qui reste réellement exclusif à cmd/veleroops-operator,
// c'est le pilotage des Restore eux-mêmes et la suppression du volume.
//
// Les deux ClusterRoles sont posés : sans veleroops-operator-rbac.yaml, un
// refus ne prouverait rien — il pourrait venir d'un droit qu'AUCUN des deux
// ClusterRoles n'accorde à personne, pas d'une frontière entre eux.
func TestOperatorRBACCannotDoVeleroOpsWork(t *testing.T) {
	ctx := context.Background()
	admin := adminClient(t)
	applyOperatorRBAC(t, ctx, admin)
	applyVeleroOpsRBAC(t, ctx, admin)

	for _, cas := range []struct {
		verbe, groupe, ressource, pourquoi string
	}{
		{"create", "velero.io", "restores",
			"cet opérateur ne crée jamais de Restore — c'est le domaine de cmd/veleroops-operator"},
		{"list", "velero.io", "restores",
			"il ne les liste pas non plus (InspectInterruptedRestore n'est jamais appelé ici)"},
		{"get", "velero.io", "backups",
			"il ne lit jamais un backup par son nom — GetVeleroBackupPhase est un appel de cmd/veleroops-operator, cet opérateur ne fait que lister et créer"},
		{"delete", "", "persistentvolumeclaims",
			"supprimer le volume d'un vcluster pour que Velero le recrée est un geste de cmd/veleroops-operator"},
	} {
		t.Run(cas.verbe+" "+cas.ressource, func(t *testing.T) {
			if allowed, raison := subjectMayDo(t, ctx, admin, operatorServiceAccount, cas.verbe, cas.groupe, cas.ressource); allowed {
				t.Fatalf("le ClusterRole de cmd/operator accorde `%s %s` (%s).\n"+
					"Ce droit appartient à cmd/veleroops-operator : %s", cas.verbe, cas.ressource, raison, cas.pourquoi)
			}
		})
	}
}

// subjectMayDo demande à l'apiserver ce qu'il répondrait pour `subject`, sans
// rien tenter.
func subjectMayDo(t *testing.T, ctx context.Context, admin ctrlclient.Client, subject, verbe, groupe, ressource string) (bool, string) {
	t.Helper()
	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User: subject,
			// Les groupes que l'apiserver ajoute lui-même à un ServiceAccount. Les
			// omettre rendrait le test plus permissif qu'il ne doit l'être : un droit
			// accordé via `system:serviceaccounts` passerait inaperçu.
			Groups: []string{
				"system:serviceaccounts",
				"system:serviceaccounts:vcluster-manager",
				"system:authenticated",
			},
			ResourceAttributes: &authzv1.ResourceAttributes{
				Verb: verbe, Group: groupe, Resource: ressource,
			},
		},
	}
	if err := admin.Create(ctx, sar); err != nil {
		t.Fatalf("SubjectAccessReview %s %s pour %s : %v", verbe, ressource, subject, err)
	}
	return sar.Status.Allowed, sar.Status.Reason
}

// --- fixtures --------------------------------------------------------------

// seedVClusterWorkloads plante ce qu'un vcluster réellement déployé porte, pour
// que la détection de topologie parte du bon côté et atteigne le Deployment du
// plan de contrôle plutôt que de s'arrêter au premier NotFound.
func seedVClusterWorkloads(t *testing.T, ctx context.Context, admin ctrlclient.Client, nom string) {
	t.Helper()
	ns := "vcluster-" + nom
	for _, obj := range []*unstructured.Unstructured{
		nsObject(ns),
		nsObject("flux-system"),
		statefulSetObject(ns, "vcluster-"+nom+"-etcd"),
		deploymentObject(ns, "vcluster-"+nom),
		pvcObject(ns, "data-vcluster-"+nom+"-etcd-0"),
		// Les deux objets Flux que SetFluxSuspend suspend. Il patche le HelmRelease
		// PUIS la Kustomization : sans le premier, le second n'est jamais émis et
		// son droit resterait non exercé.
		//
		// Le finalizer sur le HelmRelease n'est pas décoratif : CleanupNamespace ne
		// réécrit QUE les objets qui en portent un. Sans lui, l'`update` que
		// l'app et l'opérateur revendiquent tous deux sur les objets Flux ne serait
		// jamais émis, et son droit jamais vérifié.
		withFinalizer(unstructuredObject("helm.toolkit.fluxcd.io/v2", "HelmRelease", ns, "vcluster-"+nom, map[string]any{})),
		unstructuredObject("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", "tenant-"+nom, map[string]any{}),
		// Celle-ci vit dans le namespace du tenant, là où CleanupNamespace regarde
		// — la précédente est dans flux-system et lui échappe.
		withFinalizer(unstructuredObject("kustomize.toolkit.fluxcd.io/v1", "Kustomization", ns, nom+"-apps", map[string]any{})),
	} {
		if err := admin.Apply(ctx, ctrlclient.ApplyConfigurationFromUnstructured(obj),
			ctrlclient.FieldOwner("rbac-probe"), ctrlclient.ForceOwnership); err != nil {
			t.Fatalf("semis de %s/%s : %v", obj.GetKind(), obj.GetName(), err)
		}
	}
}
