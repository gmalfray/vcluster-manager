package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// Ce fichier ne teste aucune étape en particulier. Il teste ce qui se passe
// ENTRE elles.
//
// Quatre chantiers ont été menés en parallèle — sommeil, budget, status observé,
// finalizer — puis fusionnés. Chacun a sa campagne de tests, et chacune s'arrête
// au bord de son périmètre : les tests du provisionnement appellent
// `reconcileProvisioning` directement, ceux du budget appellent
// `checkResourceBudget` directement, ceux du finalizer ne passent jamais par le
// chemin vivant. Ce qu'aucun ne voit, c'est l'enchaînement complet que
// `reconcileAll` exécute en production.
//
// Plusieurs tests ci-dessous figent un DÉFAUT et le disent. Ils sont écrits
// ainsi parce qu'une suite rouge ne se relit pas : le jour où le défaut est
// corrigé, le test tombe, et c'est le signal attendu — pas une régression. Le
// commentaire de chacun dit ce qui devrait arriver à la place.

// --- un faux qui couvre TOUT le seam ---------------------------------------

// fullOps implémente les quatre interfaces que le reconciler tire de `r.Ops`
// par assertion de type. C'est ce que fait `*service.Service` en production, et
// qu'aucun faux du dépôt ne faisait : chaque campagne n'a fourni que sa moitié,
// donc chaque campagne a mesuré, sans le savoir, le comportement de l'opérateur
// avec un seam incomplet.
type fullOps struct {
	*fakeDeletionOps

	// Le cinquième seam. fullOps existe pour être le SEUL faux qui les implémente
	// tous : chaque campagne de chantier n'a fourni que sa moitié, donc chacune a
	// mesuré, sans le savoir, le comportement de l'opérateur avec un contrôleur
	// incomplet. Le chantier intégrations en a ajouté un — sans cette ligne, les
	// tests d'interaction continuaient de voir ArgoCDReady et VaultConfigured
	// absentes, c'est-à-dire exactement le faux vert qu'ils dénonçaient.
	integrations fakeIntegrationOps

	gen *gitops.Generator

	obsMu sync.Mutex
	obs   service.VClusterObservation
}

var (
	_ VClusterOps            = (*fullOps)(nil)
	_ VClusterObserver       = (*fullOps)(nil)
	_ VClusterProvisioner    = (*fullOps)(nil)
	_ VClusterDeletionOps    = (*fullOps)(nil)
	_ QuotaResolver          = (*fullOps)(nil)
	_ VClusterIntegrationOps = (*fullOps)(nil)
)

// EffectiveQuotas délègue au VRAI générateur, comme le rendu : c'est toute la
// raison d'être de ce seam — le budget et le provisionnement doivent lire la même
// règle, donc un faux qui recalculerait la sienne ne prouverait rien.
func (f *fullOps) EffectiveQuotas(req *models.CreateRequest, env string) (string, string, string, bool, error) {
	subs := f.gen.Substitutions(req, env, "")
	return subs["QUOTA_CPU"], subs["QUOTA_MEMORY"], subs["QUOTA_STORAGE"],
		subs["QUOTAS_ENABLED"] == "true", nil
}

func (f *fullOps) VaultAuthConfigured(ctx context.Context, name, env string) (bool, error) {
	return f.integrations.VaultAuthConfigured(ctx, name, env)
}

func (f *fullOps) VaultWebhookReady(ctx context.Context, name, env string) (bool, error) {
	return f.integrations.VaultWebhookReady(ctx, name, env)
}

func (f *fullOps) ConfigureVaultAuth(ctx context.Context, name, env string) error {
	return f.integrations.ConfigureVaultAuth(ctx, name, env)
}

func (f *fullOps) EnsureKeycloakClient(name, env string) error {
	return f.integrations.EnsureKeycloakClient(name, env)
}

func (f *fullOps) GetRancherStatus(ctx context.Context, name, env string) service.RancherStatus {
	return f.integrations.GetRancherStatus(ctx, name, env)
}

func (f *fullOps) PairRancher(ctx context.Context, actor models.Actor, name, env string) (service.RancherStatus, error) {
	return f.integrations.PairRancher(ctx, actor, name, env)
}

func newFullOps() *fullOps {
	return &fullOps{
		fakeDeletionOps: unpairedOps(),
		// Intégrations saines par défaut, même intention que healthyObservation() :
		// ces tests portent sur les interactions entre étapes, pas sur les pannes
		// de tiers. Un défaut « vault pas prêt » ferait échouer Ready partout et
		// masquerait ce qu'ils mesurent.
		integrations: fakeIntegrationOps{
			vaultExists:   true,
			vaultReady:    true,
			rancherStatus: service.RancherStatus{Enabled: true, Paired: true},
		},
		gen: newFakeProvisioner().gen,
		obs: healthyObservation(),
	}
}

func (f *fullOps) ObserveVCluster(_ context.Context, name, env string) service.VClusterObservation {
	f.obsMu.Lock()
	defer f.obsMu.Unlock()
	o := f.obs
	o.Name, o.Env = name, env
	return o
}

func (f *fullOps) setObservation(o service.VClusterObservation) {
	f.obsMu.Lock()
	defer f.obsMu.Unlock()
	f.obs = o
}

func (f *fullOps) RenderVClusterSubstitutions(req *models.CreateRequest, env, k8sVersion string) ([]*unstructured.Unstructured, error) {
	f.record("render")
	return []*unstructured.Unstructured{
		gitops.HostNamespace(req.Name),
		f.gen.SubstitutionConfigMap(req, env, k8sVersion),
	}, nil
}

// budgetedReconciler câble l'opérateur comme le fait cmd/operator/main.go : un
// plafond configuré, un lecteur de quotas, la garde de placement.
func budgetedReconciler(ops any) *VClusterReconciler {
	r := &VClusterReconciler{
		Client: k8sClient, Cell: "preprod", Namespace: "default",
		Budget:    BudgetLimits{CPU: "100", Memory: "400Gi", Storage: "10Ti"},
		BudgetOps: &fakeBudgetReader{},
	}
	r.Ops = ops.(VClusterServiceOps)
	return r
}

// --- provisionnement × status observé --------------------------------------
// Un refus de provisionnement survit à l'observation qui le suit, ET descend
// jusqu'à Ready.
//
// C'était le trou : reconcileProvisioning rendait `nil` sur ses trois chemins de
// refus, donc reconcileAll enchaînait, et applyObservation réécrivait
// ResourcesProvisioned depuis un cluster parfaitement sain. Le CR ressortait
// Ready=True — sur le canal exact que Flux prend comme health check.
//
// Le patron correct existait juste à côté, celui du refus de budget : signaler le
// refus à l'appelant, qui agrège et s'arrête.
//
// Appel direct à reconcileAll et pas via l'API : la règle CEL de la CRD refuse
// `type: capi` à l'admission, donc un test qui passe par Create ne ferait que se
// sauter lui-même. Ce qu'on vérifie ici est la défense en profondeur — la branche
// qui couvre un CR admis avant que la règle n'existe.
func TestARefusedProvisioningIsAggregatedIntoReady(t *testing.T) {
	ctx := context.Background()
	// budgetedReconciler : le budget passe avant le provisionnement, donc sans
	// plafond c'est lui qui refuserait et le test mesurerait autre chose.
	r := budgetedReconciler(newFullOps())

	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "capi-refus-tient", Namespace: "default"},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg", Type: v1alpha1.VClusterTypeCAPI},
	}

	if _, err := r.reconcileAll(ctx, vc); err != nil {
		t.Fatalf("reconcileAll: %v", err)
	}

	requireVCCond(t, vc, v1alpha1.CondResourcesProvisioned, metav1.ConditionFalse, "TypeNotImplemented")

	// Le cœur du test : sans agrégation sur le chemin de refus, Ready garderait sa
	// valeur du passage précédent — ici absente, en production « sain ».
	ready := vcCondition(vc, v1alpha1.CondVClusterReady)
	if ready == nil {
		t.Fatal("Ready n'a pas été agrégée sur le chemin de refus : elle garderait sa valeur " +
			"du passage précédent, et Flux verrait un vcluster sain")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %s (%s), attendu False : rien n'a été provisionné", ready.Status, ready.Reason)
	}
	if vc.Status.Phase == v1alpha1.VClusterPhaseReady {
		t.Fatal("phase Ready sur un type non implémenté")
	}
}

// Un nom invalide est maintenant refusé deux fois — et c'est devenu redondant
// pour tout objet qui passe réellement par l'API server, ce qui mérite d'être
// écrit noir sur blanc plutôt que découvert en relisant ce test.
//
// `1cluster` est le nom que ce test visait : un nom d'objet Kubernetes valide
// que nameRegex (`^[a-z][a-z0-9-]*$`) refuse. Il servait à prouver que la garde
// de placement — qui appelle `service.ValidName` avant tout provisionnement —
// rattrape un nom que Kubernetes, lui, aurait accepté. Ce n'est plus vrai : la
// règle CEL de la CRD (vcluster_name_validation_test.go,
// `TestSchemaRefusesNameStartingWithADigit`) refuse maintenant `1cluster` à
// l'admission, avant même que le CR n'existe.
//
// Le recouvrement est total, pas partiel, et il vaut la peine d'être vérifié
// plutôt que supposé : la garde n'appelle que `service.ValidName` (même charset
// que la CEL, même refus du nom réservé « manager »), et la CEL ajoute
// uniquement un plafond de longueur — un nom que la CEL admet est donc TOUJOURS
// un nom que la garde admet aussi. Il n'existe pas de nom qui passerait
// l'admission et que la garde rattraperait encore. C'est le même constat que
// TestVClusterNamedAfterTheOperatorNamespaceIsRefusedAtAdmission /
// TestGuardRefusesTheReservedNameOnItsOwn (namespace_guard_test.go) ont déjà
// fait pour le nom réservé « manager » — même patron ici, généralisé à la forme
// du nom.
//
// Ce qui reste à couvrir, et que l'admission ne peut PAS couvrir : un CR dont le
// nom est invalide mais déjà dans etcd — créé avant que la règle CEL n'existe,
// ou sur une CRD pas encore redéployée. La garde doit continuer à le rattraper
// SEULE, sans dépendre de l'admission : on l'appelle donc directement, comme
// TestGuardRefusesTheReservedNameOnItsOwn, plutôt que de faire passer l'objet
// par un k8sClient.Create() que la CEL refuserait désormais.
func TestTheGuardStillRefusesAnInvalidNameEvenThoughAdmissionCatchesItToo(t *testing.T) {
	vc := &v1alpha1.VCluster{ObjectMeta: metav1.ObjectMeta{Name: "1cluster", Namespace: "default"}}
	if reason := vclusterMisplaced(vc, "default"); reason == "" {
		t.Fatal("la garde accepte un nom invalide : un CR déjà en etcd avant la règle CEL, ou sur " +
			"une CRD pas encore redéployée, ne serait plus rattrapé")
	}

	ctx := context.Background()

	// L'autre moitié : si la garde était contournée (bug, CRD non redéployée), le
	// refus du provisionnement ne doit pas être effacé par l'observation qui suit.
	// Appel direct à reconcileAll, puisque plus rien de vivant n'y mène.
	direct := &v1alpha1.VCluster{ObjectMeta: metav1.ObjectMeta{Name: "1direct", Namespace: "default"}}
	blind := &observerOverride{fakeProvisioner: newFakeProvisioner(), obs: service.VClusterObservation{}}
	r2 := budgetedReconciler(blind)
	if _, err := r2.reconcileAll(ctx, direct); err != nil {
		t.Fatalf("reconcileAll: %v", err)
	}
	if c := vcCondition(direct, v1alpha1.CondResourcesProvisioned); c == nil || c.Reason != "InvalidName" {
		t.Fatalf("ResourcesProvisioned = %+v : le refus a été effacé par l'observation qui suit", c)
	}
	if c := vcCondition(direct, v1alpha1.CondVClusterReady); c != nil && c.Status == metav1.ConditionTrue {
		t.Fatal("Ready=True sur un nom refusé : Flux verrait un vcluster sain")
	}
}

// observerOverride force l'observation rendue par fakeProvisioner.
type observerOverride struct {
	*fakeProvisioner
	obs service.VClusterObservation
}

func (o *observerOverride) ObserveVCluster(_ context.Context, name, env string) service.VClusterObservation {
	got := o.obs
	got.Name, got.Env = name, env
	return got
}

// Un provisionneur qui ne rend AUCUN objet (fakeObserver simule ce cas plutôt
// qu'un seam manquant : voir plus bas) n'applique rien, et l'observation qui
// suit annonce quand même ResourcesProvisioned/Healthy — ce n'est pas une
// panne, c'est le cluster qui répond que tout va bien pour ce qu'on lui a
// demandé de surveiller.
//
// Ce test visait avant un vrai seam manquant : fakeObserver n'implémentait
// alors ni VClusterObserver ni VClusterProvisioner, et reconcileProvisioning
// posait une condition RendererUnavailable que l'observation qui suivait
// effaçait aussitôt — « une absence se lit comme un succès ». Ce chemin n'est
// plus atteignable : r.Ops est VClusterServiceOps, l'union des six seams, donc
// fakeObserver implémente les deux pour de vrai (RenderVClusterSubstitutions
// rend juste zéro objet). La condition RendererUnavailable elle-même a disparu
// du code de production — il n'y a plus de branche qui l'écrit — donc rien ne
// pourrait plus la faire réapparaître ici.
func TestAMissingProvisionerStillReportsProvisioned(t *testing.T) {
	ctx := context.Background()
	ops := &fakeObserver{obs: healthyObservation()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "sans-provisionneur", nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Rien n'a été appliqué : le namespace du vcluster n'existe pas.
	var ns corev1.Namespace
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "vcluster-sans-provisionneur"}, &ns); !apierrors.IsNotFound(err) {
		t.Fatalf("préalable : le namespace ne devait pas exister (err=%v)", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionTrue, "Healthy")
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "AllChecksPassed")
}

// Un bloc `quotas` absent est provisionné ET imputé au budget.
//
// C'était la contradiction : createRequestFromCR faisait
// `NoQuotas = Quotas != nil && !Enabled` — bloc absent vaut quotas ACTIFS, aux
// valeurs par défaut du générateur, ce que le commentaire de QuotaSpec revendique
// explicitement — tandis que checkResourceBudget faisait
// `Quotas == nil || !Enabled`, soit « rien à imputer ».
//
// Un CR sans bloc obtenait donc un ResourceQuota sur la cell que le plafond ne
// comptait jamais à l'admission, et qui comptait ensuite contre les vclusters
// SUIVANTS — SumVClusterQuotas somme les ResourceQuota des namespaces vcluster-*.
// Omettre trois lignes suffisait à contourner « le contrôle qui compte » d'ADR-001.
//
// Tranché dans le sens que le commentaire de QuotaSpec annonçait : le budget
// impute désormais le quota EFFECTIF, celui qui sera réellement écrit, et il le
// demande à la même source que le provisionnement.
func TestQuotasBlockAbsentIsProvisionedAndBilled(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()
	r := budgetedReconciler(ops)

	vc := newProvisioningVCluster(t, ctx, "quota-fantome", v1alpha1.VClusterSpec{})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cm := getSubstitutions(t, ctx, "quota-fantome")
	if cm.Data["QUOTAS_ENABLED"] != "true" || cm.Data["QUOTA_CPU"] == "" {
		t.Fatalf("préalable : un bloc absent doit provisionner des quotas (ENABLED=%q CPU=%q)",
			cm.Data["QUOTAS_ENABLED"], cm.Data["QUOTA_CPU"])
	}

	got := fetchVCluster(t, ctx, vc)
	c := vcCondition(got, v1alpha1.CondBudgetOK)
	if c == nil {
		t.Fatal("BudgetOK absente")
	}
	if c.Reason == "NoQuotaRequested" {
		t.Fatalf("BudgetOK = %s/NoQuotaRequested alors que QUOTA_CPU=%s est provisionné : "+
			"le quota est écrit mais jamais imputé, donc il consommera le plafond des "+
			"vclusters suivants sans être passé devant le contrôle", c.Status, cm.Data["QUOTA_CPU"])
	}
}

// Un refus de budget ne se réveille jamais tout seul.
//
// crd-vcluster.md §4.1 point 2 demande explicitement un « Requeue périodique (le
// budget peut se libérer si un autre vcluster est supprimé entretemps) ».
// reconcileAll rendait `0, nil` : aucun requeue. Et le reconciler ne surveille
// que les VCluster (`For(&VCluster{})`), donc la suppression d'un VOISIN — qui est
// justement ce qui libère la place — ne produit aucun événement sur celui-ci.
//
// Le vcluster refusé restait donc en Failed jusqu'à ce que quelqu'un touche son
// spec ou redémarre l'opérateur, même quand la place s'était libérée depuis
// longtemps. C'est le scénario normal de la file d'attente (« je crée, ça ne
// rentre pas, je supprime un vieux, ça devrait repartir ») qui ne fonctionnait
// pas.
//
// Corrigé : le refus demande un requeue périodique.
func TestABudgetRefusalRequeuesItself(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()
	r := &VClusterReconciler{
		Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default",
		Budget:    BudgetLimits{CPU: "32"},
		BudgetOps: &fakeBudgetReader{used: models.BudgetUsage{CPU: qty("30")}},
	}

	vc := newProvisioningVCluster(t, ctx, "budget-endormi", v1alpha1.VClusterSpec{
		Quotas: &v1alpha1.QuotaSpec{Enabled: true, CPU: "8"},
	})
	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondBudgetOK, metav1.ConditionFalse, "BudgetExceeded")
	if got.Status.Phase != v1alpha1.VClusterPhaseFailed {
		t.Fatalf("phase = %q, attendu Failed", got.Status.Phase)
	}
	if res.RequeueAfter != BudgetRetryInterval {
		t.Fatalf("requeue %s, attendu %s : sans requeue le refus ne se réévalue jamais tout "+
			"seul, et la suppression du voisin qui libère la place ne produit aucun événement "+
			"sur ce CR", res.RequeueAfter, BudgetRetryInterval)
	}
}

// Un vcluster qui tourne et que le budget refuse continue d'être observé, et
// n'est pas déclaré en panne.
//
// Le budget est revérifié à chaque passage, pas seulement à la création : baisser
// RESOURCE_BUDGET_CPU, ou voir un voisin grossir, suffit à faire basculer un
// vcluster déjà provisionné et sain. reconcileAll rendait alors la main, donc
// l'observation ne tournait plus : chartVersion, usage des quotas, état Rancher et
// dernier backup se figeaient, et Ready passait à False — le health check de la
// Kustomization Flux devenait rouge pour un vcluster qui va parfaitement bien.
//
// « Refuser d'allouer plus » et « déclarer en panne » ne sont pas la même chose.
// Le budget est un contrôle d'admission, pas un interrupteur d'extinction : la
// condition BudgetOK reste visible et dit ce qui se passe, elle ne prétend plus
// que le vcluster est cassé.
func TestALiveVClusterRefusedByTheBudgetKeepsBeingObserved(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()
	reader := &fakeBudgetReader{used: models.BudgetUsage{CPU: qty("10")}}
	r := &VClusterReconciler{
		Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default",
		Budget: BudgetLimits{CPU: "32"}, BudgetOps: reader,
	}

	vc := newProvisioningVCluster(t, ctx, "budget-baisse", v1alpha1.VClusterSpec{
		Quotas: &v1alpha1.QuotaSpec{Enabled: true, CPU: "8"},
	})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	before := fetchVCluster(t, ctx, vc)
	requireVCCond(t, before, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "AllChecksPassed")
	if before.Status.ChartVersion != "0.20.0" {
		t.Fatalf("préalable : chartVersion = %q", before.Status.ChartVersion)
	}

	// L'exploitation baisse le plafond de la cell.
	r.Budget = BudgetLimits{CPU: "12"}
	// Et le vcluster monte de version entre-temps : ça doit se voir.
	moved := healthyObservation()
	moved.ChartVersion = "0.21.0"
	ops.setObservation(moved)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	after := fetchVCluster(t, ctx, vc)

	// Le dépassement est dit.
	requireVCCond(t, after, v1alpha1.CondBudgetOK, metav1.ConditionFalse, "BudgetExceeded")

	// Mais le vcluster n'est ni aveugle ni déclaré cassé.
	if after.Status.ChartVersion != "0.21.0" {
		t.Fatalf("chartVersion = %q, attendu 0.21.0 : l'observation ne tourne plus, l'état du "+
			"vcluster se fige à sa dernière valeur connue", after.Status.ChartVersion)
	}
	if c := vcCondition(after, v1alpha1.CondVClusterReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v, attendu True : le health check de la Kustomization Flux devient "+
			"rouge pour un vcluster qui va bien", c)
	}
	if after.Status.Phase == v1alpha1.VClusterPhaseFailed {
		t.Fatal("phase Failed sur un vcluster debout et sain : trop gros pour la cell n'est pas en panne")
	}
}

// --- l'agrégat × les étapes que personne n'a écrites -------------------------

// ArgoCD activé : Ready exige désormais qu'on ait regardé.
//
// C'était le faux vert. La règle « une condition absente veut dire que l'étape
// n'a pas encore tourné » est juste tant qu'une étape finira par tourner — et il
// n'y en avait aucune : ni ArgoCDReady ni VaultConfigured n'étaient écrites par
// qui que ce soit, donc l'agrégat les sautait et un vcluster demandant ArgoCD
// était déclaré prêt sans que rien d'ArgoCD n'ait été vérifié. C'est ce Ready que
// le health check de Flux lit.
//
// Réserve assumée, portée par le message de la condition et non cachée derrière
// un True optimiste : ArgoCDReady ne couvre aujourd'hui que le volet Keycloak. Le
// dépôt GitLab et la santé de la Kustomization ArgoCD ne sont pas encore vérifiés.
func TestArgoCDEnabledIsNotReadyUntilArgoCDIsChecked(t *testing.T) {
	ctx := context.Background()
	spec := v1alpha1.VClusterSpec{
		ArgoCD: &v1alpha1.ArgoCDSpec{Enabled: true, RBACGroups: []string{"team-a"}},
	}

	// 1. Keycloak en panne : Ready ne doit pas être True.
	casse := newFullOps()
	casse.integrations.keycloakErr = errors.New("keycloak injoignable")
	rCasse := budgetedReconciler(casse)
	vcCasse := newProvisioningVCluster(t, ctx, "argo-keycloak-casse", spec)

	// L'échec Keycloak remonte en erreur de réconciliation, c'est voulu : il sera
	// réessayé. Ce qui compte est que le status ne prétende pas que tout va bien.
	_, _ = rCasse.Reconcile(ctx, vcReq(vcCasse))

	gotCasse := fetchVCluster(t, ctx, vcCasse)
	requireVCCond(t, gotCasse, v1alpha1.CondArgoCDReady, metav1.ConditionFalse, "KeycloakClientFailed")
	if c := vcCondition(gotCasse, v1alpha1.CondVClusterReady); c != nil && c.Status == metav1.ConditionTrue {
		t.Fatalf("Ready=True alors que le client OIDC ArgoCD n'a pas pu être créé : %+v", c)
	}

	// 2. Tout sain : les deux conditions sont écrites, et Ready peut être True.
	sain := newFullOps()
	rSain := budgetedReconciler(sain)
	vcSain := newProvisioningVCluster(t, ctx, "argo-verifie", spec)
	if _, err := rSain.Reconcile(ctx, vcReq(vcSain)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	gotSain := fetchVCluster(t, ctx, vcSain)
	for _, condType := range []string{v1alpha1.CondArgoCDReady, v1alpha1.CondVaultConfigured} {
		c := vcCondition(gotSain, condType)
		if c == nil {
			t.Fatalf("%s absente : personne ne l'écrit, donc l'agrégat la saute et Ready ne "+
				"veut rien dire pour ce volet", condType)
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("%s = %s/%s", condType, c.Status, c.Reason)
		}
	}
	requireVCCond(t, gotSain, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "AllChecksPassed")
}

// --- garde de placement × finalizer ----------------------------------------

// Un CR refusé par la garde qui s'en va doit pouvoir partir.
//
// La garde passe avant tout, y compris avant le chemin de suppression — pour
// qu'un CR déposé n'importe où ne puisse pas déclencher le finalizer du vcluster
// homonyme. Écrite naïvement, cette règle coince en Terminating pour toujours un
// CR légitime dont le namespace autorisé change sous ses pieds : plus personne
// pour retirer son finalizer, et il faut le faire à la main.
//
// C'est corrigé : refuser d'AGIR sur un objet n'oblige pas à refuser de le
// LÂCHER. Test de non-régression sur ce correctif — et sur sa limite, vérifiée
// juste après.
func TestARefusedVClusterOnItsWayOutIsReleased(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()

	// L'opérateur d'hier acceptait "default".
	vc := newDeletingVCluster(t, ctx, "namespace-deplace", false, nil)

	// L'overlay est modifié : le namespace autorisé n'est plus le même.
	reconfigured := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "vcluster-manager"}
	res, err := reconfigured.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("requeue %s : la garde ne requeue pas", res.RequeueAfter)
	}

	if !vclusterGone(t, ctx, vc) {
		got := fetchVCluster(t, ctx, vc)
		t.Fatalf("objet toujours retenu (finalizers %v) : un CR refusé qui s'en va reste "+
			"coincé en Terminating, et il faut sortir un kubectl patch pour le libérer",
			got.Finalizers)
	}
}

// La limite du correctif ci-dessus, et elle est sérieuse : libérer un objet
// refusé veut dire ne PAS jouer sa séquence de suppression.
//
// Ce qui déclenche le refus n'est pas forcément une attaque. Un
// `--vclusters-namespace` mal renseigné dans l'overlay refuse tous les CR de la
// cell. Sur le chemin vivant c'est visible tout de suite — plus rien ne se
// provisionne. Sur le chemin de suppression, non : chaque CR supprimé pendant
// cette fenêtre disparaît proprement SANS dépairage Rancher, SANS sauvegarde
// Velero d'avant destruction, SANS retrait de la protection de namespace. Le
// garde-fou de données le plus important du chantier est désarmé par une faute
// de frappe dans un flag, et rien ne le dit.
//
// TROU CONNU. Attendu : la libération d'un objet refusé mériterait au moins un
// Event Kubernetes et une ligne d'audit, pour que « j'ai lâché un vcluster sans
// jouer sa séquence » ne soit pas indiscernable d'une suppression normale.
func TestReleasingARefusedVClusterSkipsTheWholeSafetySequence(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()
	// Tout est en place pour que la séquence ait du travail : Rancher connaît le
	// cluster, le namespace est protégé, aucune sauvegarde n'existe.
	ops.rancher = service.RancherTeardownState{Enabled: true, StillKnown: true}
	ops.protection = service.ProtectionState{Available: true, Protected: true}

	vc := newDeletingVCluster(t, ctx, "sequence-sautee", false, nil)

	misconfigured := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "namespace-mal-orthographie"}
	if _, err := misconfigured.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !vclusterGone(t, ctx, vc) {
		t.Fatal("objet retenu : la garde ne libère plus, relire ce test")
	}
	if trace := ops.trace(); len(trace) != 0 {
		t.Fatalf("la séquence a tourné (%v) : la garde joue désormais la suppression avant "+
			"de lâcher, ce test a fait son temps", trace)
	}
	t.Log("CR supprimé sans dépairage Rancher, sans sauvegarde et sans retrait de la " +
		"protection de namespace — sur un simple flag --vclusters-namespace erroné")
}

// Le pendant sur le chemin vivant : un CR devenu mal placé garde le Ready qu'il
// avait avant, donc Flux continue de le voir sain.
//
// La garde écrit Accepted=False et rend la main sans toucher ni à Ready ni à la
// phase. Or Ready est précisément ce que le health check de la Kustomization
// Flux consulte (crd-vcluster.md §3.3) : un CR que l'opérateur a cessé de
// piloter reste vert.
//
// TROU CONNU. Attendu : refuser un objet devrait au minimum faire tomber Ready
// à Unknown — « je ne pilote plus ceci » n'est pas « tout va bien ».
func TestAMisplacedVClusterKeepsItsStaleReadyCondition(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()

	accepting := budgetedReconciler(ops)
	vc := newProvisioningVCluster(t, ctx, "ready-perime", v1alpha1.VClusterSpec{})
	if _, err := accepting.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile initial: %v", err)
	}
	requireVCCond(t, fetchVCluster(t, ctx, vc), v1alpha1.CondVClusterReady, metav1.ConditionTrue, "")

	reconfigured := budgetedReconciler(ops)
	reconfigured.Namespace = "vcluster-manager"
	if _, err := reconfigured.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile après reconfiguration: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondAccepted, metav1.ConditionFalse, "NamespaceMismatch")
	ready := vcCondition(got, v1alpha1.CondVClusterReady)
	if ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %s : le refus retombe désormais sur l'agrégat, ce test a fait son temps", ready.Status)
	}
	if got.Status.Phase != v1alpha1.VClusterPhaseReady {
		t.Fatalf("phase = %q, ce test fige Ready", got.Status.Phase)
	}
}

// --- seam manquant : plus un scénario atteignable ---------------------------
//
// Il y avait ici TestAMissingDeletionSeamLeavesNothingInTheStatus, qui posait
// un faux n'implémentant « ni observateur, ni provisionneur, ni suppression »
// comme `r.Ops` et vérifiait que le trou se refermait silencieusement. C'était
// la trace directe de la dette décrite dans docs/etat-brique-operateur.md
// « Dette assumée » : quatre (en réalité six) interfaces récupérées par
// assertion de type sur un champ `Ops VClusterOps` trop étroit, chacune
// dégradant le cas `!ok` à sa façon plutôt que le refuser.
//
// r.Ops est maintenant VClusterServiceOps (vcluster_controller.go), l'union des
// six. Un faux qui n'implémente pas l'un d'eux ne COMPILE plus contre ce champ
// — il n'y a donc plus de test à écrire ici : le compilateur fait le travail
// qu'un test d'exécution faisait avant lui. fakeVClusterOps lui-même ne fait
// plus exception : il implémente désormais les six (vcluster_controller_test.go),
// avec des panics pour les cinq seams qu'il ne sert qu'à faire compiler, jamais
// à exercer.

// --- la borne de sauvegarde : une ancre pour deux significations ------------

// Une sauvegarde qui vient d'être lancée peut être déclarée « jamais apparue
// après 2 h » dans la seconde qui suit.
//
// `overdue` s'ancre sur la LastTransitionTime de la condition, et cette date ne
// bouge que quand le STATUT change — pas la raison. Or BackupCompleted porte
// deux Unknown de sens opposés : BackupUnknown (« Velero est illisible ») et
// InProgress (« une sauvegarde est partie »). Passer du premier au second ne
// déplace pas l'ancre, donc la nouvelle sauvegarde hérite de l'âge de la panne
// de lecture qui la précédait.
//
// Scénario : Velero est indisponible pendant plus de deux heures, revient, la
// séquence lance enfin une sauvegarde — et le tour suivant conclut qu'elle a
// expiré. La suppression se retrouve bloquée sur BackupTimedOut avec, comme
// seule issue, l'annotation qui autorise à détruire sans filet.
//
// TROU CONNU. Attendu : la borne d'attente d'une sauvegarde devrait s'ancrer
// sur le moment où la sauvegarde a été lancée, pas sur celui où la condition a
// pris le statut Unknown.
func TestABackupJustLaunchedCanInheritAnOldTimeout(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps() // aucune sauvegarde connue de Velero
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := newDeletingVCluster(t, ctx, "ancre-partagee", false, nil)

	// Velero a été illisible pendant plus longtemps que la borne.
	ageCondition(t, ctx, vc, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionUnknown,
		"BackupUnknown", backupGiveUpAfter+time.Hour)

	// Velero répond de nouveau, ne connaît aucune sauvegarde : on en lance une.
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if n := ops.count("trigger-backup"); n != 1 {
		t.Fatalf("sauvegarde lancée %d fois, attendu 1", n)
	}
	requireVClusterCond(t, fetchVCluster(t, ctx, vc), v1alpha1.CondVClusterBackupCompleted,
		metav1.ConditionUnknown, reasonBackupProgress)

	// Velero ne la montre pas encore — le cas normal, elle vient de partir.
	ops.mu.Lock()
	ops.backup = service.DeletionBackupState{}
	ops.mu.Unlock()

	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	c := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondVClusterBackupCompleted)
	if c.Reason != "BackupTimedOut" {
		t.Fatalf("BackupCompleted = %s/%s : la borne ne réutilise plus l'ancre de la panne "+
			"de lecture, le trou est bouché — retirer ce test", c.Status, c.Reason)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("requeue %s : ce test fige l'abandon", res.RequeueAfter)
	}
}

// --- sommeil × finalizer ----------------------------------------------------

// Supprimer un vcluster déjà endormi doit marcher. C'est le parcours normal du
// modèle de suppression en trois commits (§4.2) : suspend, délai de grâce, puis
// disparition du fichier du CR.
//
// Le piège est que le vcluster est à zéro réplique quand la séquence démarre :
// le job rancher-cleanup ne peut ni être déposé ni être lu, donc l'étape
// Rancher doit conclure sur autre chose qu'une attente. Aucun test ne joignait
// les deux moitiés jusqu'ici.
func TestASleepingVClusterCanStillBeDeleted(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()
	// Dépairé côté Rancher, mais le vcluster est éteint : le nettoyage interne
	// n'est pas observable.
	ops.rancher = service.RancherTeardownState{
		Enabled: true,
		Cleanup: service.CleanupJobState{Observable: false},
	}
	ops.backup = service.DeletionBackupState{
		Found: true, Name: "b-sommeil", Phase: "Completed", Completed: true, StartedAt: time.Now(),
	}
	r := budgetedReconciler(ops)

	vc := newProvisioningVCluster(t, ctx, "sommeil-supprime", v1alpha1.VClusterSpec{Suspend: true})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("mise en sommeil: %v", err)
	}
	if got := fetchVCluster(t, ctx, vc); got.Status.Phase != v1alpha1.VClusterPhaseSuspended {
		t.Fatalf("phase = %q, attendu Suspended", got.Status.Phase)
	}

	if err := k8sClient.Delete(ctx, fetchVCluster(t, ctx, vc)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile de suppression: %v", err)
	}

	if !vclusterGone(t, ctx, vc) {
		t.Fatalf("un vcluster endormi n'a pas pu être supprimé (trace: %v)", ops.trace())
	}
	if n := ops.count("teardown"); n != 1 {
		t.Fatalf("teardown appelé %d fois, attendu 1", n)
	}
}

// --- ce que « Destroying » détruit ------------------------------------------

// Il y avait ici TestTheDeletionSequenceLeavesTheHostNamespaceBehind, qui figeait
// le défaut que l'arbitrage N6 a corrigé : le CR partait, le namespace restait.
//
// Il a été retiré parce qu'il était devenu un vert qui ne mesure rien. Le
// finalizer supprime désormais le namespace, mais `fakeDeletionOps` le simule
// avec un booléen en mémoire : le namespace du cluster de test n'était plus
// touché par personne, donc le test constatait qu'il « survit » et passait —
// pour la mauvaise raison, en continuant d'annoncer un trou refermé.
//
// La propriété se vérifie maintenant contre le vrai kube-apiserver, avec un faux
// dont les deux méthodes de namespace tapent sur le cluster :
// TestTheOperatorDeletesTheHostNamespaceItProvisioned et
// TestTheCRIsReleasedOnceTheNamespaceIsReallyGone, dans
// vcluster_namespace_removal_test.go.

// --- l'override de sauvegarde accepte désormais n'importe quelle valeur ------

// Une valeur qui veut dire non ne désarme pas le filet.
//
// C'était une régression du correctif qui a fait porter à l'annotation le nom du
// décideur : en passant de `== "true"` à `!= ""`, toute valeur non vide s'est mise
// à désarmer, y compris « false », « no », « 0 », « non ». Quelqu'un qui posait
// l'annotation à "false" en croyant refuser obtenait exactement l'inverse, et la
// ligne d'audit enregistrait « sauvegarde sautée sur décision de false ».
//
// Un garde-fou qu'on lève en écrivant « non » est pire que pas de garde-fou : il
// donne l'impression d'avoir refusé.
func TestBackupOverrideRejectsValuesThatMeanNo(t *testing.T) {
	for _, valeur := range []string{"false", "FALSE", " false ", "no", "non", "0", "off"} {
		t.Run(valeur, func(t *testing.T) {
			ctx := context.Background()
			ops := unpairedOps()
			ops.triggerErr = errors.New("velero pas installé")
			r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

			vc := newDeletingVCluster(t, ctx, "override-negatif-"+strings.ToLower(strings.TrimSpace(valeur)),
				false, map[string]string{v1alpha1.AnnDeletionBackupOverride: valeur})

			if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if vclusterGone(t, ctx, vc) {
				t.Fatalf("détruit sans sauvegarde sur backup-override=%q : une valeur de "+
					"négation a désarmé le garde-fou", valeur)
			}
			if n := ops.count("trigger-backup"); n == 0 {
				t.Fatal("l'étape de sauvegarde a été court-circuitée : l'override a été pris pour un oui")
			}
		})
	}
}

// Et le pendant positif, sinon les cas ci-dessus passeraient aussi avec un
// override qui ne fonctionne plus du tout.
func TestBackupOverrideStillDisarmsWhenItNamesSomeone(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.triggerErr = errors.New("velero pas installé")
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := newDeletingVCluster(t, ctx, "override-nomme", false, map[string]string{
		v1alpha1.AnnDeletionBackupOverride: "greg",
	})

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatal("l'override nommé ne désarme plus : le geste de déblocage n'existe plus")
	}
	if n := ops.count("trigger-backup"); n != 0 {
		t.Fatalf("une sauvegarde a été tentée (%d) alors que l'override devait court-circuiter", n)
	}
}

// Le pendant : une annotation présente mais vide ne doit pas désarmer. C'est la
// seule valeur que la nouvelle forme refuse encore, donc autant la verrouiller.
func TestAnEmptyBackupOverrideStillBlocks(t *testing.T) {
	ctx := context.Background()
	ops := unpairedOps()
	ops.triggerErr = errors.New("velero pas installé")
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "cell1", Namespace: "default"}

	vc := newDeletingVCluster(t, ctx, "override-vide", false, map[string]string{
		v1alpha1.AnnDeletionBackupOverride: "",
	})

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("détruit sans sauvegarde sur une annotation vide")
	}
	requireVClusterCond(t, fetchVCluster(t, ctx, vc), v1alpha1.CondVClusterBackupCompleted,
		metav1.ConditionFalse, "BackupTriggerFailed")
}

// Un opérateur sans clients d'intégration rend Ready=Unknown, jamais True.
//
// C'est le pendant côté déploiement de N4, et c'est le cas ACTUEL :
// cmd/operator/main.go ne câble ni Vault, ni Keycloak, ni Rancher. Les étapes
// écrivent donc Unknown/NotConfigured, ce qui est honnête — mais comme
// VaultConfigured et ArgoCDReady sont bloquantes, l'agrégat sort Unknown et le
// health check de la Kustomization Flux ne passe jamais au vert.
//
// Ce test fige cette conséquence pour qu'elle soit un fait écrit et non une
// surprise : soit on câble les identifiants sur l'opérateur, soit on accepte que
// tout vcluster demandant ArgoCD reste Unknown. Le choix a un coût de sécurité —
// donner à ce pod le token GitLab, le secret client Keycloak et les creds Vault
// élargit ce qu'une compromission emporte — donc il ne se décide pas ici.
func TestAnOperatorWithoutIntegrationClientsIsUnknownNotReady(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()
	ops.integrations.vaultExistsErr = service.ErrVaultNotConfigured
	ops.integrations.keycloakErr = service.ErrKeycloakNotConfigured
	r := budgetedReconciler(ops)

	vc := newProvisioningVCluster(t, ctx, "sans-integrations", v1alpha1.VClusterSpec{
		ArgoCD: &v1alpha1.ArgoCDSpec{Enabled: true},
	})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondVaultConfigured, metav1.ConditionUnknown, "VaultNotConfigured")
	requireVCCond(t, got, v1alpha1.CondArgoCDReady, metav1.ConditionUnknown, "KeycloakNotConfigured")

	ready := vcCondition(got, v1alpha1.CondVClusterReady)
	if ready == nil {
		t.Fatal("Ready absente")
	}
	if ready.Status == metav1.ConditionTrue {
		t.Fatalf("Ready=True (%s) alors qu'aucune intégration n'a pu être vérifiée : "+
			"c'est le faux vert de N4, une couche plus bas", ready.Reason)
	}
	if ready.Status != metav1.ConditionUnknown {
		t.Errorf("Ready = %s/%s, attendu Unknown : rien n'est cassé, on n'a simplement "+
			"pas pu regarder", ready.Status, ready.Reason)
	}
}
