package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeObserver adds the read half of the seam to the suspend/resume fake. It
// hands back a scripted observation, which is the only thing that separates
// these tests from a live cluster: the aggregation logic underneath is the real
// one, running against a real API server through envtest.
type fakeObserver struct {
	fakeVClusterOps

	obsMu sync.Mutex
	obs   service.VClusterObservation
	calls int
}

func (f *fakeObserver) ObserveVCluster(_ context.Context, name, env string) service.VClusterObservation {
	f.obsMu.Lock()
	defer f.obsMu.Unlock()
	f.calls++
	o := f.obs
	o.Name, o.Env = name, env
	return o
}

// RenderVClusterSubstitutions fait de ce faux un provisionneur en plus d'un
// observateur, sans rien à appliquer.
//
// Nécessaire depuis qu'un refus de provisionnement coupe la réconciliation : un
// faux qui n'implémente que l'observation déclenchait RendererUnavailable, donc
// un refus, donc ces tests mesuraient l'agrégation d'un opérateur cassé au lieu
// de l'agrégation d'un opérateur sain. C'est exactement le défaut que la recette
// a nommé — chaque campagne n'a fourni que sa moitié du seam.
func (f *fakeObserver) RenderVClusterSubstitutions(*models.CreateRequest, string, string) ([]*unstructured.Unstructured, error) {
	return nil, nil
}

func (f *fakeObserver) setObservation(o service.VClusterObservation) {
	f.obsMu.Lock()
	defer f.obsMu.Unlock()
	f.obs = o
}

// healthyObservation is a vcluster where every source answered and everything is
// fine. Each test spoils exactly one thing, so what it proves is unambiguous.
func healthyObservation() service.VClusterObservation {
	return service.VClusterObservation{
		HelmRelease:     "Ready",
		Kustomization:   "Ready",
		ChartVersion:    "0.20.0",
		K8sVersion:      "v1.31.4",
		CPUUsage:        "1.2/4",
		MemoryUsage:     "3Gi/8Gi",
		StorageUsage:    "10Gi/50Gi",
		RancherEnabled:  true,
		RancherKnown:    true,
		RancherState:    service.RancherStatePaired,
		RancherPaired:   true,
		ProtectionKnown: true,
		BackupsKnown:    true,
		LastBackup: &models.VeleroBackupInfo{
			Name:           "vcluster-demo-20260806",
			Phase:          "Completed",
			CompletionTime: "2026-08-06T02:11:00Z",
		},
	}
}

func newObservedVCluster(t *testing.T, ctx context.Context, name string, mutate func(*v1alpha1.VCluster)) *v1alpha1.VCluster {
	t.Helper()
	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg"},
	}
	if mutate != nil {
		mutate(vc)
	}
	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), vc) })
	return vc
}

// seedCondition writes a condition another reconcile step would have written,
// through the status subresource, so the aggregation reads it back the way it
// would in production.
func seedCondition(t *testing.T, ctx context.Context, vc *v1alpha1.VCluster, condType string, status metav1.ConditionStatus, reason, message string) {
	t.Helper()
	got := fetchVCluster(t, ctx, vc)
	setVClusterCond(got, condType, status, reason, message)
	if err := k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("seed condition %s: %v", condType, err)
	}
}

func vcCondition(vc *v1alpha1.VCluster, condType string) *metav1.Condition {
	for i := range vc.Status.Conditions {
		if vc.Status.Conditions[i].Type == condType {
			return &vc.Status.Conditions[i]
		}
	}
	return nil
}

func requireVCCond(t *testing.T, vc *v1alpha1.VCluster, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := vcCondition(vc, condType)
	if c == nil {
		t.Fatalf("condition %s absente (conditions: %+v)", condType, vc.Status.Conditions)
	}
	if c.Status != status {
		t.Fatalf("condition %s : status %s, attendu %s (reason %s, message %q)", condType, c.Status, status, c.Reason, c.Message)
	}
	if reason != "" && c.Reason != reason {
		t.Fatalf("condition %s : reason %s, attendu %s", condType, c.Reason, reason)
	}
}

// Le cas nominal : tout a répondu, tout va bien. Ready est l'agrégat que Flux
// prend comme health check, donc c'est lui qu'on vérifie en premier, avec le
// status observé qui va avec.
func TestReadyWhenEveryObservedSourceIsHealthy(t *testing.T) {
	ctx := context.Background()
	ops := &fakeObserver{obs: healthyObservation()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "observe-sain", nil)
	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "AllChecksPassed")
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionTrue, "Healthy")
	requireVCCond(t, got, v1alpha1.CondRancherPaired, metav1.ConditionTrue, "Paired")
	if got.Status.Phase != v1alpha1.VClusterPhaseReady {
		t.Fatalf("phase = %q, attendu Ready", got.Status.Phase)
	}
	if got.Status.ChartVersion != "0.20.0" || got.Status.K8sVersion != "v1.31.4" {
		t.Fatalf("versions non reportées : chart=%q k8s=%q", got.Status.ChartVersion, got.Status.K8sVersion)
	}
	if got.Status.ResourceUsage == nil || got.Status.ResourceUsage.Memory != "3Gi/8Gi" {
		t.Fatalf("usage quotas non reporté : %+v", got.Status.ResourceUsage)
	}
	if got.Status.LastBackup == nil || got.Status.LastBackup.CompletedAt == nil {
		t.Fatalf("dernier backup non reporté : %+v", got.Status.LastBackup)
	}
	if res.RequeueAfter != ObserveIntervalSettled {
		t.Fatalf("re-scrutation dans %s, attendu %s : rien ne bouge, inutile de marteler l'API", res.RequeueAfter, ObserveIntervalSettled)
	}
}

// Rancher muet : le piège central. Le vcluster tourne, seul l'appel Rancher a
// échoué. Il ne doit ni passer pour cassé, ni passer pour dépairé.
func TestUnreachableRancherIsUnknownNotUnpaired(t *testing.T) {
	ctx := context.Background()
	ops := &fakeObserver{obs: healthyObservation()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "rancher-muet", nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if got := fetchVCluster(t, ctx, vc); !got.Status.Rancher.Paired {
		t.Fatal("préalable : le vcluster devait être vu appairé au premier passage")
	}

	blind := healthyObservation()
	blind.RancherKnown = false
	blind.RancherState = service.RancherStateUnknown
	blind.RancherPaired = false
	ops.setObservation(blind)

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondRancherPaired, metav1.ConditionUnknown, "LookupFailed")
	if got.Status.Rancher.State != service.RancherStateUnknown {
		t.Fatalf("rancher.state = %q, attendu Unknown", got.Status.Rancher.State)
	}
	if !got.Status.Rancher.Paired {
		t.Fatal("rancher.paired repassé à false alors que Rancher n'a pas répondu : ça se lit « dépairé » et invite à réappairer un cluster qui l'est déjà")
	}
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "AllChecksPassed")
	if got.Status.Phase != v1alpha1.VClusterPhaseReady {
		t.Fatalf("phase = %q : une panne Rancher a fait passer le vcluster pour cassé", got.Status.Phase)
	}
}

// Le cluster hôte ne répond plus. On perd la vue, pas les faits : le status
// vieillit, il ne s'efface pas, et le reconcile ne tombe pas.
func TestUnreachableClusterDegradesWithoutErasingWhatWasKnown(t *testing.T) {
	ctx := context.Background()
	ops := &fakeObserver{obs: healthyObservation()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "cluster-muet", nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	ops.setObservation(service.VClusterObservation{Err: service.ErrK8sUnavailable})
	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("une source injoignable a fait échouer le reconcile : %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionUnknown, "NoClusterClient")
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionUnknown, "ResourcesProvisionedUnknown")
	if got.Status.Phase != v1alpha1.VClusterPhaseDegraded {
		t.Fatalf("phase = %q, attendu Degraded", got.Status.Phase)
	}
	if got.Status.ChartVersion != "0.20.0" || got.Status.K8sVersion != "v1.31.4" {
		t.Fatalf("valeurs effacées par une lecture ratée : chart=%q k8s=%q — un chartVersion qui disparaît se lit comme une désinstallation", got.Status.ChartVersion, got.Status.K8sVersion)
	}
	if got.Status.LastBackup == nil {
		t.Fatal("dernier backup effacé alors qu'on n'a rien pu lire")
	}
	if got.Status.Rancher.State != service.RancherStatePaired {
		t.Fatalf("rancher.state = %q : personne n'a interrogé Rancher, la dernière valeur connue devait rester", got.Status.Rancher.State)
	}
	if res.RequeueAfter != ObserveIntervalMoving {
		t.Fatalf("re-scrutation dans %s, attendu %s : être aveugle est exactement le moment où il ne faut pas attendre", res.RequeueAfter, ObserveIntervalMoving)
	}
}

// « Inconnu » ne vaut pas « faux » dans l'agrégat non plus : Ready doit sortir
// Unknown, pas False. False dit « constaté en échec », et on n'a rien constaté.
func TestUnknownNeverBecomesFalseInTheAggregate(t *testing.T) {
	ctx := context.Background()
	obs := healthyObservation()
	obs.HelmRelease = "Unknown"
	obs.Kustomization = "Unknown"
	ops := &fakeObserver{obs: obs}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "inconnu-pas-faux", nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionUnknown, "NotReadable")
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionUnknown, "")
}

// Un échec constaté l'emporte sur une inconnue : entre « le HelmRelease est en
// échec » et « la Kustomization est illisible », c'est le problème connu qu'on
// affiche.
func TestAKnownFailureWinsOverAnUnknown(t *testing.T) {
	ctx := context.Background()
	obs := healthyObservation()
	obs.HelmRelease = "UpgradeFailed"
	obs.Kustomization = "Unknown"
	ops := &fakeObserver{obs: obs}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "echec-vs-inconnu", nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionFalse, "NotReady")
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "ResourcesProvisionedNotMet")
}

// Une condition bloquante posée par une autre étape descend jusqu'à Ready. Le
// budget refusé est le seul cas qui vaut Failed : rien ne sera provisionné tant
// que le plafond n'aura pas bougé (crd-vcluster.md §4.1).
func TestBudgetRefusedMakesReadyFalseAndPhaseFailed(t *testing.T) {
	ctx := context.Background()
	ops := &fakeObserver{obs: healthyObservation()}
	// Un VRAI refus, pas une condition semée : l'étape budget réécrit BudgetOK à
	// chaque passage, donc en semer une serait la tester contre elle-même.
	// Plafond dépassé = 30 déjà alloués sur la cell + 8 demandés > 32.
	r := &VClusterReconciler{
		Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default",
		Budget:    BudgetLimits{CPU: "32"},
		BudgetOps: &fakeBudgetReader{used: models.BudgetUsage{CPU: qty("30")}},
	}

	vc := newObservedVCluster(t, ctx, "budget-refuse", func(v *v1alpha1.VCluster) {
		v.Spec.Quotas = &v1alpha1.QuotaSpec{Enabled: true, CPU: "8"}
	})

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "BudgetOKNotMet")
	if got.Status.Phase != v1alpha1.VClusterPhaseFailed {
		t.Fatalf("phase = %q, attendu Failed", got.Status.Phase)
	}
}

// Une condition ArgoCD restée d'une époque où ArgoCD était activé ne doit pas
// bloquer Ready une fois ArgoCD coupé.
func TestArgoCDConditionIgnoredWhenArgoCDIsOff(t *testing.T) {
	ctx := context.Background()
	ops := &fakeObserver{obs: healthyObservation()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "argocd-coupe", nil)
	seedCondition(t, ctx, vc, v1alpha1.CondArgoCDReady, metav1.ConditionFalse, "KeycloakFailed",
		"client OIDC absent")

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := fetchVCluster(t, ctx, vc); vcCondition(got, v1alpha1.CondVClusterReady).Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %s : une condition ArgoCD périmée bloque un vcluster sans ArgoCD", vcCondition(got, v1alpha1.CondVClusterReady).Status)
	}

	// Et la même condition compte dès qu'ArgoCD est demandé.
	withArgo := newObservedVCluster(t, ctx, "argocd-actif", func(vc *v1alpha1.VCluster) {
		vc.Spec.ArgoCD = &v1alpha1.ArgoCDSpec{Enabled: true}
	})
	seedCondition(t, ctx, withArgo, v1alpha1.CondArgoCDReady, metav1.ConditionFalse, "KeycloakFailed",
		"client OIDC absent")
	if _, err := r.Reconcile(ctx, vcReq(withArgo)); err != nil {
		t.Fatalf("reconcile argocd: %v", err)
	}
	requireVCCond(t, fetchVCluster(t, ctx, withArgo), v1alpha1.CondVClusterReady, metav1.ConditionFalse, "ArgoCDReadyNotMet")
}

// La première installation et la régression donnent la même lecture. Ce qui les
// sépare, c'est de l'avoir déjà vu debout — chartVersion est ce témoin.
func TestFirstInstallIsProvisioningAndARegressionIsDegraded(t *testing.T) {
	ctx := context.Background()
	installing := healthyObservation()
	installing.HelmRelease = "Progressing"
	installing.ChartVersion = ""
	installing.K8sVersion = ""
	ops := &fakeObserver{obs: installing}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "premiere-install", nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if got := fetchVCluster(t, ctx, vc); got.Status.Phase != v1alpha1.VClusterPhaseProvisioning {
		t.Fatalf("phase = %q pendant la première install, attendu Provisioning", got.Status.Phase)
	}

	// Il monte.
	ops.setObservation(healthyObservation())
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if got := fetchVCluster(t, ctx, vc); got.Status.Phase != v1alpha1.VClusterPhaseReady {
		t.Fatalf("phase = %q, attendu Ready", got.Status.Phase)
	}

	// Il retombe : même lecture qu'au premier passage, mais cette fois c'est une
	// régression.
	regression := healthyObservation()
	regression.HelmRelease = "Progressing"
	ops.setObservation(regression)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if got := fetchVCluster(t, ctx, vc); got.Status.Phase != v1alpha1.VClusterPhaseDegraded {
		t.Fatalf("phase = %q après régression, attendu Degraded", got.Status.Phase)
	}
}

// Lecture qui traîne : le reconcile ne doit pas tomber avec, et surtout la
// protection ne doit pas se lire « retirée » parce que le namespace n'a pas
// répondu.
func TestSlowReadDegradesWithoutDroppingTheProtectionFlag(t *testing.T) {
	ctx := context.Background()
	protected := healthyObservation()
	protected.Protected = true
	ops := &fakeObserver{obs: protected}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "lecture-lente", func(vc *v1alpha1.VCluster) {
		vc.Spec.DeletionProtection = true
	})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if got := fetchVCluster(t, ctx, vc); !got.Status.ProtectionEnabled {
		t.Fatal("préalable : la protection devait être vue active")
	}

	// Une lecture qui n'aboutit pas rend les valeurs vides, pas des valeurs
	// fausses : c'est exactement ce qu'il ne faut pas recopier par-dessus ce
	// qu'on savait.
	slow := healthyObservation()
	slow.ClusterTimedOut = true
	slow.ChartVersion = ""
	slow.K8sVersion = ""
	slow.CPUUsage, slow.MemoryUsage, slow.StorageUsage = "", "", ""
	slow.Protected = false // ce que GetNamespaceProtection rend quand il ne peut pas lire
	ops.setObservation(slow)

	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("une lecture qui traîne a fait échouer le reconcile : %v", err)
	}
	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionUnknown, "ReadTimedOut")
	if !got.Status.ProtectionEnabled {
		t.Fatal("protectionEnabled retombé à false sur une lecture qui n'a pas abouti")
	}
	if got.Status.ChartVersion != "0.20.0" || got.Status.K8sVersion != "v1.31.4" {
		t.Fatalf("versions écrasées par une lecture vide : chart=%q k8s=%q", got.Status.ChartVersion, got.Status.K8sVersion)
	}
	if got.Status.ResourceUsage == nil || got.Status.ResourceUsage.Memory != "3Gi/8Gi" {
		t.Fatalf("usage quotas écrasé par une lecture vide : %+v", got.Status.ResourceUsage)
	}
	requireVCCond(t, got, v1alpha1.CondDeletionProtected, metav1.ConditionTrue, "SpecOnly")
	if res.RequeueAfter != ObserveIntervalMoving {
		t.Fatalf("re-scrutation dans %s, attendu %s", res.RequeueAfter, ObserveIntervalMoving)
	}
}

// Le namespace ne dit pas la même chose que le spec : le finalizer relit le spec
// au dernier moment (§4.3), donc la divergence mérite d'être visible avant.
func TestProtectionDivergenceIsReported(t *testing.T) {
	ctx := context.Background()
	obs := healthyObservation()
	obs.Protected = false
	ops := &fakeObserver{obs: obs}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "protection-divergente", func(vc *v1alpha1.VCluster) {
		vc.Spec.DeletionProtection = true
	})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondDeletionProtected, metav1.ConditionTrue, "NamespaceDiverges")
	// Informatif seulement : une divergence d'annotation ne rend pas le vcluster
	// malade.
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "")
}

// Un appairage en cours se suit au rythme court même quand le reste est prêt :
// sinon le spinner reste affiché cinq minutes après la fin.
func TestPairingKeepsTheShortInterval(t *testing.T) {
	ctx := context.Background()
	obs := healthyObservation()
	obs.RancherState = service.RancherStatePairing
	obs.RancherPaired = false
	ops := &fakeObserver{obs: obs}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "appairage-en-cours", nil)
	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != ObserveIntervalMoving {
		t.Fatalf("re-scrutation dans %s pendant un appairage, attendu %s", res.RequeueAfter, ObserveIntervalMoving)
	}
	requireVCCond(t, fetchVCluster(t, ctx, vc), v1alpha1.CondVClusterReady, metav1.ConditionTrue, "")
}

// Réconcilier trois fois sur un état stable ne doit rien faire bouger : ni la
// phase, ni les dates de transition des conditions. Sans ça, chaque passage
// écrirait dans etcd et le dashboard clignoterait.
func TestObservationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	ops := &fakeObserver{obs: healthyObservation()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "observe-idempotent", nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	first := fetchVCluster(t, ctx, vc)
	firstTransition := vcCondition(first, v1alpha1.CondVClusterReady).LastTransitionTime

	time.Sleep(10 * time.Millisecond)
	for i := range 2 {
		if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
			t.Fatalf("reconcile %d: %v", i+2, err)
		}
	}

	got := fetchVCluster(t, ctx, vc)
	if got.Status.Phase != first.Status.Phase {
		t.Fatalf("phase passée de %q à %q sans que rien ne change", first.Status.Phase, got.Status.Phase)
	}
	if !vcCondition(got, v1alpha1.CondVClusterReady).LastTransitionTime.Equal(&firstTransition) {
		t.Fatal("lastTransitionTime de Ready bougé alors que le status n'a pas changé")
	}
	if len(got.Status.Conditions) != len(first.Status.Conditions) {
		t.Fatalf("%d conditions puis %d : des doublons s'accumulent", len(first.Status.Conditions), len(got.Status.Conditions))
	}
}

// Rancher désactivé sur la cell n'est pas une panne : c'est un fait, et il ne
// doit pas empêcher Ready.
func TestRancherDisabledIsAFactNotAFailure(t *testing.T) {
	ctx := context.Background()
	obs := healthyObservation()
	obs.RancherEnabled = false
	obs.RancherPaired = false
	obs.RancherState = service.RancherStateOff
	ops := &fakeObserver{obs: obs}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "rancher-off", nil)
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondRancherPaired, metav1.ConditionFalse, "Disabled")
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "")
}

// Un vcluster en sommeil ne se fait pas observer : le status ne doit pas
// annoncer « dégradé » pour quelque chose qu'on a volontairement couché.
func TestSuspendedVClusterIsNotObserved(t *testing.T) {
	ctx := context.Background()
	ops := &fakeObserver{obs: healthyObservation()}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "observe-sommeil", func(vc *v1alpha1.VCluster) {
		vc.Spec.Suspend = true
	})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ops.obsMu.Lock()
	calls := ops.calls
	ops.obsMu.Unlock()
	if calls != 0 {
		t.Fatalf("%d observation(s) sur un vcluster en sommeil", calls)
	}
	if got := fetchVCluster(t, ctx, vc); got.Status.Phase != v1alpha1.VClusterPhaseSuspended {
		t.Fatalf("phase = %q, attendu Suspended", got.Status.Phase)
	}
}

// Sans observateur branché, l'opérateur dit qu'il ne sait pas — il n'invente pas
// un verdict et il ne tombe pas.
// provisionneurSansObservateur isole l'absence d'observateur. Il faut un
// provisionneur, sinon c'est le refus de provisionnement qui coupe la
// réconciliation et le test mesure autre chose que ce qu'il annonce.
type provisionneurSansObservateur struct{ fakeVClusterOps }

func (*provisionneurSansObservateur) RenderVClusterSubstitutions(*models.CreateRequest, string, string) ([]*unstructured.Unstructured, error) {
	return nil, nil
}

func TestNoObserverReportsUnknownRatherThanFailing(t *testing.T) {
	ctx := context.Background()
	ops := &provisionneurSansObservateur{}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newObservedVCluster(t, ctx, "sans-observateur", nil)
	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionUnknown, "NoObserver")
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionUnknown, "")
	if res.RequeueAfter != ObserveIntervalMoving {
		t.Fatalf("re-scrutation dans %s, attendu %s", res.RequeueAfter, ObserveIntervalMoving)
	}
}
