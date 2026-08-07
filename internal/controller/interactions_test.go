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

	gen *gitops.Generator

	obsMu sync.Mutex
	obs   service.VClusterObservation
}

var (
	_ VClusterOps         = (*fullOps)(nil)
	_ VClusterObserver    = (*fullOps)(nil)
	_ VClusterProvisioner = (*fullOps)(nil)
	_ VClusterDeletionOps = (*fullOps)(nil)
)

func newFullOps() *fullOps {
	return &fullOps{
		fakeDeletionOps: unpairedOps(),
		gen:             newFakeProvisioner().gen,
		obs:             healthyObservation(),
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
	r.Ops = ops.(VClusterOps)
	return r
}

// --- provisionnement × status observé --------------------------------------

// Un refus de provisionnement doit survivre à l'étape qui passe juste après.
//
// L'observation a « le dernier mot » sur ResourcesProvisioned, par choix
// délibéré : c'est ce que le cluster montre qui fait foi. Mais un refus n'est
// pas une observation périmée, c'est une décision — et elle est réécrite par un
// pas qui ne sait rien du refus qu'il efface.
//
// TROU CONNU : le refus disparaît. Un CR `type: capi` ressort Ready/True,
// ResourcesProvisioned/Healthy, phase Ready, alors que reconcileProvisioning
// vient de poser TypeNotImplemented et n'a rien provisionné. Attendu : le refus
// devrait couper reconcileAll comme le fait le refus de budget, qui appelle
// l'agrégation lui-même et rend la main.
func TestCAPIRefusalIsErasedByTheObservationStep(t *testing.T) {
	ctx := context.Background()
	ops := newFakeProvisioner() // rend une observation saine
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newVCluster("capi-efface", func(v *v1alpha1.VCluster) {
		v.Spec.Type = v1alpha1.VClusterTypeCAPI
	})

	if _, err := r.reconcileAll(ctx, vc); err != nil {
		t.Fatalf("reconcileAll: %v", err)
	}

	c := vcCondition(vc, v1alpha1.CondResourcesProvisioned)
	if c == nil {
		t.Fatal("ResourcesProvisioned absente")
	}
	if c.Reason == "TypeNotImplemented" {
		t.Fatal("le refus a survécu à l'observation — le trou est bouché, retirer ce test")
	}
	if c.Status != metav1.ConditionTrue {
		t.Fatalf("ResourcesProvisioned = %s/%s : ce test fige le cas où l'observation "+
			"écrase le refus par un True ; s'il change, relire pourquoi", c.Status, c.Reason)
	}
	if got := vcCondition(vc, v1alpha1.CondVClusterReady); got.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %s, ce test fige True", got.Status)
	}
	if vc.Status.Phase != v1alpha1.VClusterPhaseReady {
		t.Fatalf("phase = %q, ce test fige Ready : un type non implémenté est annoncé sain "+
			"à Flux, dont le health check lit précisément Ready", vc.Status.Phase)
	}
}

// Le même effacement pour un nom refusé — sauf que ce chemin-là est désormais
// fermé en amont, et c'est le bon endroit.
//
// `1cluster` est un nom d'objet Kubernetes valide que nameRegex
// (`^[a-z][a-z0-9-]*$`) refuse. Avant, le CR était admis, reconcileProvisioning
// le refusait avec InvalidName, et l'observation effaçait le refus : le vcluster
// restait en Provisioning sans que personne ne sache pourquoi. La garde de
// placement valide maintenant le nom AVANT reconcileAll, donc reconcileProvisioning
// ne le voit plus jamais.
//
// Ce test garde les deux moitiés : la garde refuse (non-régression), et la branche
// InvalidName de reconcileProvisioning serait toujours effacée si on l'atteignait
// (le trou de fond, pas encore bouché — voir le test CAPI juste au-dessus).
func TestAnInvalidNameIsStoppedByTheGuardBeforeProvisioning(t *testing.T) {
	ctx := context.Background()
	ops := newFakeProvisioner()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "1cluster", Namespace: "default"},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg"},
	}
	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("préalable : Kubernetes devait accepter ce nom : %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), vc) })

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondAccepted, metav1.ConditionFalse, "NamespaceMismatch")
	if ops.renderCall != 0 {
		t.Fatal("le nom refusé a été transmis au rendu")
	}
	if c := vcCondition(got, v1alpha1.CondResourcesProvisioned); c != nil {
		t.Fatalf("ResourcesProvisioned = %s/%s : le reconcile est allé plus loin que la garde",
			c.Status, c.Reason)
	}

	// L'autre moitié : si la branche InvalidName était atteinte, l'observation
	// l'effacerait toujours. Appel direct, puisque plus rien n'y mène.
	direct := &v1alpha1.VCluster{ObjectMeta: metav1.ObjectMeta{Name: "1direct", Namespace: "default"}}
	blind := &observerOverride{fakeProvisioner: newFakeProvisioner(), obs: service.VClusterObservation{}}
	r2 := &VClusterReconciler{Client: k8sClient, Ops: blind, Cell: "preprod", Namespace: "default"}
	if _, err := r2.reconcileAll(ctx, direct); err != nil {
		t.Fatalf("reconcileAll: %v", err)
	}
	if c := vcCondition(direct, v1alpha1.CondResourcesProvisioned); c.Reason == "InvalidName" {
		t.Fatal("le refus survit maintenant à l'observation : le trou de fond est bouché, " +
			"simplifier ce test")
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

// Un faux qui n'implémente pas VClusterProvisioner ne provisionne rien, et
// l'observation qui suit annonce ResourcesProvisioned/Healthy.
//
// C'est la forme la plus pure de « une absence se lit comme un succès » : le
// seam manque, personne n'a appliqué le namespace ni le ConfigMap de
// substitutions, et le CR affiche Ready. La condition RendererUnavailable que
// reconcileProvisioning prend soin de poser vit le temps d'une fonction.
//
// En production les deux assertions de type réussissent, donc ce chemin
// n'existe pas — c'est justement ce qui le rend dangereux dans les TESTS : un
// double partiel dégrade au lieu d'échouer, et la campagne mesure autre chose
// que ce qu'elle croit.
func TestAMissingProvisionerStillReportsProvisioned(t *testing.T) {
	ctx := context.Background()
	ops := &fakeObserver{obs: healthyObservation()} // observateur, PAS provisionneur
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
	c := vcCondition(got, v1alpha1.CondResourcesProvisioned)
	if c.Reason == "RendererUnavailable" {
		t.Fatal("la condition du seam manquant a survécu — le trou est bouché, retirer ce test")
	}
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionTrue, "Healthy")
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "AllChecksPassed")
}

// --- provisionnement × budget ----------------------------------------------

// Les deux étapes lisent `spec.quotas` avec des conventions opposées.
//
//	createRequestFromCR : bloc absent ⇒ NoQuotas=false ⇒ QUOTAS_ENABLED=true,
//	                      avec les valeurs par défaut du générateur.
//	checkResourceBudget : bloc absent ⇒ « aucun quota demandé, rien à imputer ».
//
// Un CR sans bloc `quotas` obtient donc un ResourceQuota sur la cell — 8 CPU,
// 32Gi, 500Gi par défaut — que le budget de la cell ne compte jamais à
// l'admission. Il comptera en revanche dans le total lu pour les vclusters
// SUIVANTS (SumVClusterQuotas somme les ResourceQuota des namespaces
// vcluster-*), donc il consomme le plafond des autres sans avoir eu à passer
// devant lui. Omettre trois lignes du CR suffit à contourner « le contrôle qui
// compte » d'ADR-001.
//
// TROU CONNU. Attendu : une des deux conventions doit céder — soit un bloc
// absent vaut quotas actifs partout (et le budget doit alors imputer les
// valeurs par défaut du générateur), soit il vaut pas de quotas partout.
func TestQuotasBlockAbsentIsProvisionedButNeverBilled(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()
	r := budgetedReconciler(ops)

	vc := newProvisioningVCluster(t, ctx, "quota-fantome", v1alpha1.VClusterSpec{})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cm := getSubstitutions(t, ctx, "quota-fantome")
	if cm.Data["QUOTAS_ENABLED"] != "true" {
		t.Fatalf("QUOTAS_ENABLED = %q : ce test suppose qu'un bloc quotas absent "+
			"provisionne quand même des quotas", cm.Data["QUOTAS_ENABLED"])
	}
	if cm.Data["QUOTA_CPU"] == "" {
		t.Fatal("QUOTA_CPU vide : le générateur devait poser sa valeur par défaut")
	}

	got := fetchVCluster(t, ctx, vc)
	c := vcCondition(got, v1alpha1.CondBudgetOK)
	if c.Reason != "NoQuotaRequested" {
		t.Fatalf("BudgetOK = %s/%s, ce test fige NoQuotaRequested : si la raison a changé, "+
			"la contradiction entre les deux étapes a peut-être été tranchée", c.Status, c.Reason)
	}
	t.Logf("provisionné avec QUOTA_CPU=%s / QUOTA_MEMORY=%s / QUOTA_STORAGE=%s, imputé au budget : rien",
		cm.Data["QUOTA_CPU"], cm.Data["QUOTA_MEMORY"], cm.Data["QUOTA_STORAGE"])
}

// Un refus de budget ne se réveille jamais tout seul.
//
// crd-vcluster.md §4.1 point 2 demande explicitement un « Requeue périodique (le
// budget peut se libérer si un autre vcluster est supprimé entretemps) ».
// reconcileAll rend `0, nil` : aucun requeue. Et le reconciler ne surveille que
// les VCluster (`For(&VCluster{})`), donc la suppression d'un VOISIN ne produit
// aucun événement sur celui-ci.
//
// Conséquence : un vcluster refusé pour dépassement reste en Failed jusqu'à ce
// que quelqu'un touche son spec ou redémarre l'opérateur — même quand la place
// s'est libérée depuis longtemps. C'est le scénario normal de la file d'attente
// (« je crée, ça ne rentre pas, je supprime un vieux, ça devrait repartir »).
//
// TROU CONNU : l'exigence du document n'est pas implémentée.
func TestABudgetRefusalNeverRequeuesItself(t *testing.T) {
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
	if res.RequeueAfter != 0 {
		t.Fatalf("requeue %s : le refus se réévalue enfin tout seul, §4.1 est respecté — "+
			"retirer ce test", res.RequeueAfter)
	}
}

// Un vcluster qui tourne et que le budget vient de refuser cesse d'être observé.
//
// Le budget est revérifié à chaque passage, pas seulement à la création : baisser
// RESOURCE_BUDGET_CPU, ou voir un voisin grossir, suffit à faire basculer un
// vcluster déjà provisionné et sain. reconcileAll rend la main sur le refus, donc
// l'observation ne tourne plus : chartVersion, usage des quotas, état Rancher et
// dernier backup se figent à la dernière valeur connue, et Ready passe à False —
// c'est-à-dire que le health check de la Kustomization Flux devient rouge pour un
// vcluster qui, lui, va parfaitement bien.
//
// Rien n'est détruit, et c'est le bon choix. Mais « refuser d'allouer plus » et
// « déclarer en panne » ne sont pas la même chose, et le CR ne dit que la
// seconde.
func TestALiveVClusterRefusedByTheBudgetStopsBeingObserved(t *testing.T) {
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
	// Et le vcluster a monté de version entre-temps : personne ne le verra.
	moved := healthyObservation()
	moved.ChartVersion = "0.21.0"
	ops.setObservation(moved)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	after := fetchVCluster(t, ctx, vc)
	requireVCCond(t, after, v1alpha1.CondBudgetOK, metav1.ConditionFalse, "BudgetExceeded")
	requireVCCond(t, after, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "BudgetOKNotMet")
	if after.Status.Phase != v1alpha1.VClusterPhaseFailed {
		t.Fatalf("phase = %q, attendu Failed", after.Status.Phase)
	}
	if after.Status.ChartVersion != "0.20.0" {
		t.Fatalf("chartVersion = %q : l'observation tourne désormais malgré le refus de "+
			"budget, relire ce test", after.Status.ChartVersion)
	}
}

// --- l'agrégat × les étapes que personne n'a écrites -------------------------

// ArgoCD activé, Ready quand même : aucune étape de l'opérateur ne pose
// ArgoCDReady, donc la condition est absente, donc l'agrégat la saute.
//
// La règle « une condition absente veut dire que l'étape n'a pas encore tourné »
// est juste tant qu'une étape finira par tourner. Ici il n'y en a aucune :
// Keycloak, le dépôt GitLab et la Kustomization ArgoCD ne sont vérifiés par
// personne dans l'opérateur, et VaultConfigured est dans le même cas. Un
// vcluster qui demande ArgoCD est donc déclaré prêt sans que rien d'ArgoCD
// n'ait été regardé — et c'est ce Ready que le health check de Flux lit.
//
// TROU CONNU, de couverture fonctionnelle plutôt que de code : les étapes
// §4.1 point 4 (Keycloak / GitLab / Vault) ne sont pas portées.
func TestArgoCDEnabledIsReadyWithoutAnybodyCheckingArgoCD(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()
	r := budgetedReconciler(ops)

	vc := newProvisioningVCluster(t, ctx, "argo-non-verifie", v1alpha1.VClusterSpec{
		ArgoCD: &v1alpha1.ArgoCDSpec{Enabled: true, RBACGroups: []string{"team-a"}},
	})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	if c := vcCondition(got, v1alpha1.CondArgoCDReady); c != nil {
		t.Fatalf("ArgoCDReady = %s/%s : quelqu'un écrit enfin cette condition, "+
			"ce test a fait son temps", c.Status, c.Reason)
	}
	if c := vcCondition(got, v1alpha1.CondVaultConfigured); c != nil {
		t.Fatalf("VaultConfigured = %s/%s : idem", c.Status, c.Reason)
	}
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "AllChecksPassed")
	if got.Status.Phase != v1alpha1.VClusterPhaseReady {
		t.Fatalf("phase = %q, ce test fige Ready", got.Status.Phase)
	}
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

// --- seam manquant sur le chemin destructeur --------------------------------

// Le seam de suppression absent bloque l'objet SANS rien écrire nulle part.
//
// Les trois assertions de type sur `r.Ops` traitent la même panne de trois
// façons : VClusterObserver pose Unknown/NoObserver, VClusterProvisioner pose
// False/RendererUnavailable, VClusterDeletionOps retourne une erreur avant
// d'avoir écrit quoi que ce soit. Sur le chemin le plus grave des trois, c'est
// la variante la plus muette : l'objet reste en Terminating, `kubectl describe`
// ne dit rien, et l'erreur ne sort que dans les logs du pod.
func TestAMissingDeletionSeamLeavesNothingInTheStatus(t *testing.T) {
	ctx := context.Background()
	ops := &fakeVClusterOps{} // ni observateur, ni provisionneur, ni suppression
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newDeletingVCluster(t, ctx, "seam-manquant", false, nil)
	_, err := r.Reconcile(ctx, vcReq(vc))
	if err == nil {
		t.Fatal("aucune erreur : l'absence du seam de suppression est passée inaperçue")
	}

	got := fetchVCluster(t, ctx, vc)
	if len(got.Status.Conditions) != 0 {
		t.Fatalf("conditions écrites : %+v — le trou est bouché, relire ce test", got.Status.Conditions)
	}
	if got.Status.Phase != "" {
		t.Fatalf("phase = %q : ce test fige le silence complet du status", got.Status.Phase)
	}
	if vclusterGone(t, ctx, vc) {
		t.Fatal("objet libéré alors que la séquence n'a jamais tourné")
	}
}

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

// Le namespace que l'opérateur a créé lui-même survit à la séquence de
// suppression.
//
// reconcileProvisioning applique `vcluster-<nom>` en Server-Side Apply, sans
// ownerReference — un objet cluster-scoped ne peut de toute façon pas être
// possédé par un CR namespacé. L'étape Destroying, elle, retire les finalizers
// Flux (CleanupNamespace) et nettoie les systèmes externes ; elle ne supprime
// aucun objet Kubernetes. §4.4 point 4 supposait une cascade par
// ownerReferences qui n'existe pas.
//
// Aujourd'hui c'est Flux qui prune le namespace, parce que l'arborescence
// commitée le contient encore (`clusters/<cell>/vclusters/<nom>/kustomization.yaml`
// tire `../../../../base`, qui porte le Namespace). La suppression dépend donc
// d'un commit que le finalizer n'écrit pas et ne vérifie pas — et le jour où
// l'arborescence disparaît au profit du seul CR, plus rien ne supprime le
// namespace.
//
// TROU CONNU, et c'est le point à recetter en premier sur cluster réel.
func TestTheDeletionSequenceLeavesTheHostNamespaceBehind(t *testing.T) {
	ctx := context.Background()
	ops := newFullOps()
	ops.backup = service.DeletionBackupState{
		Found: true, Name: "b-teardown", Phase: "Completed", Completed: true, StartedAt: time.Now(),
	}
	r := budgetedReconciler(ops)

	vc := newProvisioningVCluster(t, ctx, "namespace-survivant", v1alpha1.VClusterSpec{})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("provisionnement: %v", err)
	}
	var ns corev1.Namespace
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "vcluster-namespace-survivant"}, &ns); err != nil {
		t.Fatalf("préalable : le namespace devait avoir été créé : %v", err)
	}

	if err := k8sClient.Delete(ctx, fetchVCluster(t, ctx, vc)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile de suppression: %v", err)
	}
	if !vclusterGone(t, ctx, vc) {
		t.Fatalf("la séquence n'est pas allée au bout (trace: %v)", ops.trace())
	}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: "vcluster-namespace-survivant"}, &ns)
	if apierrors.IsNotFound(err) {
		t.Fatal("le namespace a été supprimé : quelque chose détruit enfin, retirer ce test")
	}
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		t.Fatal("le namespace est en Terminating : quelque chose l'a supprimé, retirer ce test")
	}
	t.Log("le CR est parti, le namespace vcluster-namespace-survivant est intact : " +
		"la destruction réelle dépend encore du prune Flux de l'arborescence commitée")
}

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
