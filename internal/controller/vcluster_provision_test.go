package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// fakeProvisioner est fakeVClusterOps plus le rendu. Il délègue au VRAI
// générateur : ce qui est simulé ici, c'est le service, pas la dérivation — un
// faux jeu de valeurs ne prouverait rien sur ce qui atterrit dans le cluster.
type fakeProvisioner struct {
	fakeVClusterOps
	gen        *gitops.Generator
	renderErr  error
	renderCall int
}

// ObserveVCluster fait de ce faux un observateur en plus d'un provisionneur.
//
// Nécessaire depuis que l'étape d'observation existe : elle passe APRÈS le
// provisionnement et a le dernier mot sur ResourcesProvisioned — c'est ce que
// dit le cluster qui compte, pas ce qu'on a demandé. Sans observateur, elle
// écraserait par un Unknown/NoObserver légitime ce que le provisionnement
// vient d'établir, et ces tests mesureraient l'absence de seam plutôt que le
// provisionnement.
func (f *fakeProvisioner) ObserveVCluster(context.Context, string, string) service.VClusterObservation {
	return healthyObservation()
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{gen: gitops.NewGenerator(gitops.GeneratorConfig{
		BaseDomainPreprod:   "preprod.example.com",
		BaseDomainProd:      "example.com",
		TLSSecretPreprod:    "wildcard-preprod-example-com-tls",
		OIDCIssuer:          "https://keycloak.example.com/auth/realms/r",
		GitLabSSHURL:        "ssh://git@gitlab.example.com:22226",
		GitLabArgoCDPath:    "ops/argocd",
		DefaultCPU:          "8",
		DefaultMemory:       "32Gi",
		DefaultStorage:      "500Gi",
		VeleroTimezone:      "Europe/Paris",
		VClusterPodSecurity: "privileged",
		ArgoCDDefaultPolicy: "role:readonly",
	})}
}

// EffectiveQuotas délègue au VRAI générateur, comme le rendu : c'est toute la
// raison d'être de ce seam — le budget et le provisionnement doivent lire la même
// règle, donc un faux qui recalculerait la sienne ne prouverait rien.
func (f *fakeProvisioner) EffectiveQuotas(req *models.CreateRequest, env string) (string, string, string, bool, error) {
	subs := f.gen.Substitutions(req, env, "")
	return subs["QUOTA_CPU"], subs["QUOTA_MEMORY"], subs["QUOTA_STORAGE"],
		subs["QUOTAS_ENABLED"] == "true", nil
}

func (f *fakeProvisioner) RenderVClusterSubstitutions(req *models.CreateRequest, env, k8sVersion string) ([]*unstructured.Unstructured, error) {
	f.renderCall++
	if f.renderErr != nil {
		return nil, f.renderErr
	}
	if !service.ValidName(req.Name) {
		return nil, errors.New("nom invalide")
	}
	return []*unstructured.Unstructured{
		gitops.HostNamespace(req.Name),
		f.gen.SubstitutionConfigMap(req, env, k8sVersion),
	}, nil
}

var _ VClusterProvisioner = (*fakeProvisioner)(nil)

func newProvisioningVCluster(t *testing.T, ctx context.Context, name string, spec v1alpha1.VClusterSpec) *v1alpha1.VCluster {
	t.Helper()
	spec.Owner = "greg"
	vc := &v1alpha1.VCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), vc) })
	return vc
}

func getSubstitutions(t *testing.T, ctx context.Context, name string) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: "vcluster-" + name + "-substitutions", Namespace: "vcluster-" + name}
	if err := k8sClient.Get(ctx, key, &cm); err != nil {
		t.Fatalf("get ConfigMap de substitutions: %v", err)
	}
	return &cm
}

// Le reconcile pose exactement deux objets : le namespace, et le ConfigMap de
// substitutions que Flux injectera dans les templates tenant partagés. Rien
// d'autre — c'est ce qui évite un second propriétaire sur ce que Flux applique.
func TestProvisioningAppliesTheNamespaceAndTheSubstitutions(t *testing.T) {
	ctx := context.Background()
	ops := newFakeProvisioner()
	// Le CR demande des quotas, donc le budget de la cell passe avant le
	// provisionnement : sans plafond configuré l'opérateur refuse et rien n'est
	// posé (crd-vcluster.md §5.3).
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default",
		Budget: BudgetLimits{CPU: "100", Memory: "400Gi", Storage: "10Ti"}}

	vc := newProvisioningVCluster(t, ctx, "provision-base", v1alpha1.VClusterSpec{
		Velero: &v1alpha1.VeleroSpec{Enabled: true, Hour: "03:00", TTL: "30j"},
		Quotas: &v1alpha1.QuotaSpec{Enabled: true, CPU: "4", Memory: "16Gi", Storage: "200Gi"},
	})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ns corev1.Namespace
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "vcluster-provision-base"}, &ns); err != nil {
		t.Fatalf("namespace absent: %v", err)
	}

	cm := getSubstitutions(t, ctx, "provision-base")
	want := map[string]string{
		"VCLUSTER_NAME":            "provision-base",
		"VCLUSTER_CELL":            "preprod",
		"VCLUSTER_API_HOST":        "provision-base.api.preprod.example.com",
		"VCLUSTER_TARGET_REVISION": "preprod",
		"QUOTAS_ENABLED":           "true",
		"QUOTA_CPU":                "4",
		"VELERO_ENABLED":           "true",
		// "30j" côté CR, forme Velero côté substitution : la conversion est au
		// contrôleur, pas au relecteur du diff.
		"VELERO_TTL":           "720h0m0s",
		"VELERO_SCHEDULE":      "CRON_TZ=Europe/Paris 0 3 * * *",
		"ARGOCD_ENABLED":       "false",
		"ARGOCD_URL":           "",
		"VCLUSTER_K8S_VERSION": "",
	}
	for k, v := range want {
		if cm.Data[k] != v {
			t.Errorf("%s = %q, attendu %q", k, cm.Data[k], v)
		}
	}

	// True, mais la raison vient de l'observation et pas du provisionnement :
	// les deux étapes écrivent cette condition et l'observation passe en dernier,
	// délibérément — c'est ce que le cluster montre qui fait foi, pas ce qu'on a
	// demandé. Vérifier "Applied" ici reviendrait à exiger que le provisionnement
	// ait le dernier mot, donc à figer l'ordre inverse.
	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionTrue, "")
}

// Rejouer la réconciliation ne doit rien changer : ni le contenu, ni la
// resourceVersion. Une valeur volatile dans les substitutions se verrait ici,
// sous forme d'écriture à chaque tour.
func TestProvisioningIsIdempotent(t *testing.T) {
	ctx := context.Background()
	ops := newFakeProvisioner()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newProvisioningVCluster(t, ctx, "provision-idem", v1alpha1.VClusterSpec{
		ArgoCD: &v1alpha1.ArgoCDSpec{Enabled: true, RBACGroups: []string{"team-a"}},
	})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	first := getSubstitutions(t, ctx, "provision-idem").ResourceVersion

	// Un reconciler neuf, comme après un redémarrage.
	for i := range 3 {
		fresh := &VClusterReconciler{Client: k8sClient, Ops: newFakeProvisioner(), Cell: "preprod", Namespace: "default"}
		if _, err := fresh.Reconcile(ctx, vcReq(vc)); err != nil {
			t.Fatalf("reconcile %d: %v", i+2, err)
		}
	}

	after := getSubstitutions(t, ctx, "provision-idem")
	if after.ResourceVersion != first {
		t.Fatalf("resourceVersion %s → %s : le reconcile réécrit alors que rien n'a changé",
			first, after.ResourceVersion)
	}
}

// Désactiver ArgoCD vide les clés au lieu de laisser un objet orphelin derrière.
// C'est ce qui répond à l'inconnue 2 du §7 sans mécanisme de prune : il n'y a
// jamais d'objet à retirer.
func TestTurningArgoCDOffBlanksItsValues(t *testing.T) {
	ctx := context.Background()
	ops := newFakeProvisioner()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newProvisioningVCluster(t, ctx, "provision-argo", v1alpha1.VClusterSpec{
		ArgoCD: &v1alpha1.ArgoCDSpec{Enabled: true, Version: "v2.9.3", RBACGroups: []string{"team-a"}},
	})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	cm := getSubstitutions(t, ctx, "provision-argo")
	if cm.Data["ARGOCD_ENABLED"] != "true" || cm.Data["ARGOCD_URL"] == "" {
		t.Fatalf("ArgoCD activé mal rendu: %+v", cm.Data)
	}
	if !strings.Contains(cm.Data["ARGOCD_RBAC_POLICY"], "team-a") {
		t.Errorf("groupe RBAC absent de la policy: %q", cm.Data["ARGOCD_RBAC_POLICY"])
	}
	keysWhenOn := len(cm.Data)

	// Le commit qui retire le bloc argoCD du CR.
	got := fetchVCluster(t, ctx, vc)
	got.Spec.ArgoCD = nil
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update spec: %v", err)
	}
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	cm = getSubstitutions(t, ctx, "provision-argo")
	if len(cm.Data) != keysWhenOn {
		t.Fatalf("jeu de clés = %d après extinction, attendu %d : une clé absente laisserait "+
			"Flux poser le placeholder tel quel", len(cm.Data), keysWhenOn)
	}
	for _, k := range []string{"ARGOCD_URL", "ARGOCD_VERSION", "ARGOCD_CLIENT_ID", "ARGOCD_RBAC_POLICY"} {
		if cm.Data[k] != "" {
			t.Errorf("%s = %q après extinction d'ArgoCD, attendu vide", k, cm.Data[k])
		}
	}
	if cm.Data["ARGOCD_ENABLED"] != "false" {
		t.Errorf("ARGOCD_ENABLED = %q, attendu false", cm.Data["ARGOCD_ENABLED"])
	}
}

// L'opérateur est seul propriétaire de ce ConfigMap, mais s'il se met à en
// posséder plus que ce qu'il déclare, il écraserait un jour ce qu'un autre y
// ajoute. On vérifie que Server-Side Apply se limite aux clés rendues.
func TestProvisioningOnlyOwnsWhatItDeclares(t *testing.T) {
	ctx := context.Background()
	r := &VClusterReconciler{Client: k8sClient, Ops: newFakeProvisioner(), Cell: "preprod", Namespace: "default"}

	vc := newProvisioningVCluster(t, ctx, "provision-owner", v1alpha1.VClusterSpec{})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Un autre acteur ajoute une clé et change une des nôtres.
	cm := getSubstitutions(t, ctx, "provision-owner")
	cm.Data["AJOUT_EXTERIEUR"] = "à préserver"
	cm.Data["VCLUSTER_API_HOST"] = "détourné.example.com"
	if err := k8sClient.Update(ctx, cm); err != nil {
		t.Fatalf("update concurrent: %v", err)
	}

	if _, err := r.Reconcile(ctx, vcReq(vc)); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	cm = getSubstitutions(t, ctx, "provision-owner")
	if cm.Data["AJOUT_EXTERIEUR"] != "à préserver" {
		t.Error("l'opérateur a effacé une clé qu'il ne déclare pas")
	}
	if cm.Data["VCLUSTER_API_HOST"] != "provision-owner.api.preprod.example.com" {
		t.Errorf("dérive non corrigée sur une clé que l'opérateur déclare : %q", cm.Data["VCLUSTER_API_HOST"])
	}
}

// Un nom qui ne passe pas la validation ne doit jamais atteindre une
// concaténation de namespace : refus net, condition lisible, aucune écriture.
func TestProvisioningRefusesAnUnsafeName(t *testing.T) {
	ctx := context.Background()
	ops := newFakeProvisioner()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	// Le CR n'est pas créé dans l'API : ce nom ne passerait pas la validation
	// d'un objet Kubernetes non plus. On exerce la garde du reconcile, qui est
	// la dernière avant la concaténation.
	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "../kube-system", Namespace: "default"},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg"},
	}
	provisionne, err := r.reconcileProvisioning(ctx, vc)
	if err != nil {
		t.Fatalf("attendu un refus sans erreur de réconciliation, obtenu: %v", err)
	}
	if provisionne {
		t.Fatal("le refus n'est pas signalé à l'appelant : la réconciliation va enchaîner " +
			"sur l'observation, qui réécrira la condition")
	}
	if ops.renderCall != 0 {
		t.Fatal("le nom a été transmis au rendu malgré le refus")
	}
	if vc.Status.Phase != v1alpha1.VClusterPhaseFailed {
		t.Errorf("phase = %q, attendu Failed", vc.Status.Phase)
	}
	requireVCCond(t, vc, v1alpha1.CondResourcesProvisioned, metav1.ConditionFalse, "InvalidName")
}

// type: capi est réservé. La règle CEL de la CRD le refuse à l'admission ; cette
// garde couvre un CR admis avant qu'elle n'existe, et surtout elle ne provisionne
// rien.
func TestCAPITypeProvisionsNothing(t *testing.T) {
	ctx := context.Background()
	ops := newFakeProvisioner()
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "provision-capi", Namespace: "default"},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg", Type: v1alpha1.VClusterTypeCAPI},
	}
	provisionne, err := r.reconcileProvisioning(ctx, vc)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if provisionne {
		t.Fatal("le refus n'est pas signalé à l'appelant")
	}
	if ops.renderCall != 0 {
		t.Fatal("un CR capi a été rendu")
	}
	var ns corev1.Namespace
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "vcluster-provision-capi"}, &ns)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("namespace créé pour un type non implémenté (err=%v)", err)
	}
	requireVCCond(t, vc, v1alpha1.CondResourcesProvisioned, metav1.ConditionFalse, "TypeNotImplemented")
}

// Un échec de rendu doit se voir dans le status, pas seulement dans les logs :
// Reconcile rend la main sur erreur sans écrire, donc c'est provisionFailed qui
// doit persister la condition.
func TestProvisioningFailureIsVisibleInStatus(t *testing.T) {
	ctx := context.Background()
	ops := newFakeProvisioner()
	ops.renderErr = errors.New("générateur non configuré")
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newProvisioningVCluster(t, ctx, "provision-echec", v1alpha1.VClusterSpec{})
	if _, err := r.Reconcile(ctx, vcReq(vc)); err == nil {
		t.Fatal("l'échec n'a pas été remonté, donc rien ne sera réessayé")
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondResourcesProvisioned, metav1.ConditionFalse, "RenderFailed")
}
