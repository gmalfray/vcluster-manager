package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// fakeIntegrationOps scripte les trois intégrations. Comme fakeObserver et
// fakeProvisioner avant lui, il embarque fakeVClusterOps pour satisfaire le
// champ Ops (de type VClusterOps) même si ces tests n'appellent jamais
// Suspend/Resume.
type fakeIntegrationOps struct {
	fakeVClusterOps

	mu sync.Mutex

	vaultExists    bool
	vaultExistsErr error
	vaultReady     bool
	vaultReadyErr  error
	configureErr   error

	keycloakErr error

	rancherStatus service.RancherStatus
	pairErr       error

	vaultAuthCalls     int
	vaultWebhookCalls  int
	configureCalls     int
	keycloakCalls      int
	rancherStatusCalls int
	pairCalls          int
	pairEnvsSeen       []string
	pairActorsSeen     []models.Actor
}

func (f *fakeIntegrationOps) VaultAuthConfigured(_ context.Context, _, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vaultAuthCalls++
	return f.vaultExists, f.vaultExistsErr
}

func (f *fakeIntegrationOps) VaultWebhookReady(_ context.Context, _, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vaultWebhookCalls++
	return f.vaultReady, f.vaultReadyErr
}

func (f *fakeIntegrationOps) ConfigureVaultAuth(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configureCalls++
	return f.configureErr
}

func (f *fakeIntegrationOps) EnsureKeycloakClient(_, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keycloakCalls++
	return f.keycloakErr
}

func (f *fakeIntegrationOps) GetRancherStatus(_ context.Context, _, _ string) service.RancherStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rancherStatusCalls++
	return f.rancherStatus
}

func (f *fakeIntegrationOps) PairRancher(_ context.Context, actor models.Actor, name, env string) (service.RancherStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairCalls++
	f.pairEnvsSeen = append(f.pairEnvsSeen, env)
	f.pairActorsSeen = append(f.pairActorsSeen, actor)
	if f.pairErr != nil {
		return service.RancherStatus{}, f.pairErr
	}
	return service.RancherStatus{Enabled: true, Pairing: true, Name: name, Env: env}, nil
}

func (f *fakeIntegrationOps) counts() (vaultAuth, vaultWebhook, configure, keycloak, rancherStatus, pair int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vaultAuthCalls, f.vaultWebhookCalls, f.configureCalls, f.keycloakCalls, f.rancherStatusCalls, f.pairCalls
}

var _ VClusterIntegrationOps = (*fakeIntegrationOps)(nil)

func condOf(vc *v1alpha1.VCluster, condType string) *metav1.Condition {
	for i := range vc.Status.Conditions {
		if vc.Status.Conditions[i].Type == condType {
			return &vc.Status.Conditions[i]
		}
	}
	return nil
}

// --- Vault -----------------------------------------------------------------

// Vault déjà configuré : rien n'est refait, aucune régénération de token à
// chaque passage — c'est la même question que startVaultReconciler pose au
// démarrage (« le backend existe-t-il déjà ? »), jamais un flag écrit par
// l'opérateur lui-même.
func TestReconcileVault_AlreadyConfigured_SkipsSetup(t *testing.T) {
	ops := &fakeIntegrationOps{vaultExists: true}
	r := &VClusterReconciler{Cell: "preprod"}
	vc := newVCluster("vault-ok", nil)

	requeue, err := r.reconcileVault(context.Background(), ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != 0 {
		t.Fatalf("requeue = %v, attendu 0 (rien à surveiller)", requeue)
	}
	if _, _, configureCalls, _, _, _ := ops.counts(); configureCalls != 0 {
		t.Fatal("ConfigureVaultAuth appelé alors que le backend existait déjà")
	}
	c := condOf(vc, v1alpha1.CondVaultConfigured)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Configured" {
		t.Fatalf("condition VaultConfigured = %+v", c)
	}
	if vc.Status.Vault == nil || vc.Status.Vault.Status != "done" {
		t.Fatalf("status.vault = %+v, attendu done", vc.Status.Vault)
	}
}

// vault-webhook pas encore prêt : on attend, sans jamais appeler
// ConfigureVaultAuth — pas de token généré tant que le webhook ne répond pas.
func TestReconcileVault_WaitsForWebhook(t *testing.T) {
	ops := &fakeIntegrationOps{vaultExists: false, vaultReady: false}
	r := &VClusterReconciler{Cell: "preprod"}
	vc := newVCluster("vault-waiting", nil)

	requeue, err := r.reconcileVault(context.Background(), ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != RequeueInterval {
		t.Fatalf("requeue = %v, attendu %v", requeue, RequeueInterval)
	}
	if _, _, configureCalls, _, _, _ := ops.counts(); configureCalls != 0 {
		t.Fatal("ConfigureVaultAuth appelé alors que vault-webhook n'est pas prêt")
	}
	c := condOf(vc, v1alpha1.CondVaultConfigured)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "WaitingForWebhook" {
		t.Fatalf("condition VaultConfigured = %+v", c)
	}
	if vc.Status.Vault == nil || vc.Status.Vault.Status != "waiting" {
		t.Fatalf("status.vault = %+v, attendu waiting", vc.Status.Vault)
	}
}

// vault-webhook prêt, backend absent : la séquence complète tourne, et une
// fois réussie le résultat est écrit à la fois en condition et en status.vault.
func TestReconcileVault_ConfiguresOnceWebhookIsReady(t *testing.T) {
	ops := &fakeIntegrationOps{vaultExists: false, vaultReady: true}
	r := &VClusterReconciler{Cell: "prod"}
	vc := newVCluster("vault-configure", nil)

	requeue, err := r.reconcileVault(context.Background(), ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != 0 {
		t.Fatalf("requeue = %v, attendu 0", requeue)
	}
	if _, _, configureCalls, _, _, _ := ops.counts(); configureCalls != 1 {
		t.Fatalf("ConfigureVaultAuth appelé %d fois, attendu 1", configureCalls)
	}
	c := condOf(vc, v1alpha1.CondVaultConfigured)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Configured" {
		t.Fatalf("condition VaultConfigured = %+v", c)
	}
	if vc.Status.Vault == nil || vc.Status.Vault.Status != "done" {
		t.Fatalf("status.vault = %+v, attendu done", vc.Status.Vault)
	}
}

// Un échec de configuration doit se voir : condition False, status.vault en
// erreur, et l'erreur remonte pour que le reconcile réessaie.
func TestReconcileVault_SetupFailureIsVisible(t *testing.T) {
	ops := &fakeIntegrationOps{vaultExists: false, vaultReady: true, configureErr: errors.New("vault: 500")}
	r := &VClusterReconciler{Cell: "prod"}
	vc := newVCluster("vault-fail", nil)

	requeue, err := r.reconcileVault(context.Background(), ops, vc)
	if err == nil {
		t.Fatal("l'échec de configuration n'a pas été remonté")
	}
	if requeue != RequeueInterval {
		t.Fatalf("requeue = %v, attendu %v", requeue, RequeueInterval)
	}
	c := condOf(vc, v1alpha1.CondVaultConfigured)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "SetupFailed" {
		t.Fatalf("condition VaultConfigured = %+v", c)
	}
	if vc.Status.Vault == nil || vc.Status.Vault.Status != "error" {
		t.Fatalf("status.vault = %+v, attendu error", vc.Status.Vault)
	}
}

// Absence de client Vault sur cet opérateur : Unknown, pas d'erreur — ce n'est
// pas une panne à réessayer en boucle, c'est un opérateur qui n'a jamais eu
// Vault câblé.
func TestReconcileVault_NotConfiguredIsUnknownNotError(t *testing.T) {
	ops := &fakeIntegrationOps{vaultExistsErr: service.ErrVaultNotConfigured}
	r := &VClusterReconciler{Cell: "preprod"}
	vc := newVCluster("vault-absent", nil)

	requeue, err := r.reconcileVault(context.Background(), ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != 0 {
		t.Fatalf("requeue = %v, attendu 0", requeue)
	}
	if _, webhookCalls, _, _, _, _ := ops.counts(); webhookCalls != 0 {
		t.Fatal("VaultWebhookReady appelé alors qu'aucun client Vault n'est configuré")
	}
	c := condOf(vc, v1alpha1.CondVaultConfigured)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != "VaultNotConfigured" {
		t.Fatalf("condition VaultConfigured = %+v", c)
	}
}

// Une lecture Vault ratée (pas "non configuré", un vrai échec réseau) doit
// rester Unknown — pas Faux — et remonter l'erreur pour réessayer.
func TestReconcileVault_UnreachableIsUnknownAndRetried(t *testing.T) {
	ops := &fakeIntegrationOps{vaultExistsErr: errors.New("dial tcp: timeout")}
	r := &VClusterReconciler{Cell: "preprod"}
	vc := newVCluster("vault-injoignable", nil)

	requeue, err := r.reconcileVault(context.Background(), ops, vc)
	if err == nil {
		t.Fatal("l'échec de lecture n'a pas été remonté : rien ne sera réessayé")
	}
	if requeue != RequeueInterval {
		t.Fatalf("requeue = %v, attendu %v", requeue, RequeueInterval)
	}
	c := condOf(vc, v1alpha1.CondVaultConfigured)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != "VaultUnreachable" {
		t.Fatalf("condition VaultConfigured = %+v, attendu Unknown/VaultUnreachable (pas Faux)", c)
	}
}

// --- Keycloak ----------------------------------------------------------

// ArgoCD désactivé : pas de client OIDC à créer, et ça se voit explicitement
// (pas juste un True qui traîne d'avant).
func TestReconcileKeycloak_SkipsWhenArgoCDDisabled(t *testing.T) {
	ops := &fakeIntegrationOps{}
	r := &VClusterReconciler{Cell: "preprod"}
	vc := newVCluster("sans-argocd", nil)

	requeue, err := r.reconcileKeycloak(ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != 0 {
		t.Fatalf("requeue = %v, attendu 0", requeue)
	}
	if _, _, _, keycloakCalls, _, _ := ops.counts(); keycloakCalls != 0 {
		t.Fatal("EnsureKeycloakClient appelé alors qu'ArgoCD est désactivé")
	}
	c := condOf(vc, v1alpha1.CondArgoCDReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ArgoCDDisabled" {
		t.Fatalf("condition ArgoCDReady = %+v", c)
	}
}

// ArgoCD activé : le client OIDC est demandé à chaque passage — c'est
// EnsureKeycloakClient (via CreateArgoCDClients) qui vérifie lui-même s'il
// existe déjà, jamais un flag posé ici.
func TestReconcileKeycloak_CreatesClientWhenArgoCDEnabled(t *testing.T) {
	ops := &fakeIntegrationOps{}
	r := &VClusterReconciler{Cell: "prod"}
	vc := newVCluster("avec-argocd", func(v *v1alpha1.VCluster) {
		v.Spec.ArgoCD = &v1alpha1.ArgoCDSpec{Enabled: true}
	})

	requeue, err := r.reconcileKeycloak(ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != 0 {
		t.Fatalf("requeue = %v, attendu 0", requeue)
	}
	if _, _, _, keycloakCalls, _, _ := ops.counts(); keycloakCalls != 1 {
		t.Fatalf("EnsureKeycloakClient appelé %d fois, attendu 1", keycloakCalls)
	}
	c := condOf(vc, v1alpha1.CondArgoCDReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "KeycloakClientReady" {
		t.Fatalf("condition ArgoCDReady = %+v", c)
	}
}

// Absence de client Keycloak sur cet opérateur : Unknown, pas une erreur qui
// boucle en backoff — même discipline que pour Vault.
func TestReconcileKeycloak_NotConfiguredIsUnknownNotError(t *testing.T) {
	ops := &fakeIntegrationOps{keycloakErr: service.ErrKeycloakNotConfigured}
	r := &VClusterReconciler{Cell: "preprod"}
	vc := newVCluster("keycloak-absent", func(v *v1alpha1.VCluster) {
		v.Spec.ArgoCD = &v1alpha1.ArgoCDSpec{Enabled: true}
	})

	requeue, err := r.reconcileKeycloak(ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != 0 {
		t.Fatalf("requeue = %v, attendu 0", requeue)
	}
	c := condOf(vc, v1alpha1.CondArgoCDReady)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != "KeycloakNotConfigured" {
		t.Fatalf("condition ArgoCDReady = %+v", c)
	}
}

// Un échec Keycloak réel (pas "non configuré") doit être visible et réessayé.
func TestReconcileKeycloak_FailureIsVisibleAndRetried(t *testing.T) {
	ops := &fakeIntegrationOps{keycloakErr: errors.New("keycloak: 500")}
	r := &VClusterReconciler{Cell: "preprod"}
	vc := newVCluster("keycloak-fail", func(v *v1alpha1.VCluster) {
		v.Spec.ArgoCD = &v1alpha1.ArgoCDSpec{Enabled: true}
	})

	requeue, err := r.reconcileKeycloak(ops, vc)
	if err == nil {
		t.Fatal("l'échec Keycloak n'a pas été remonté")
	}
	if requeue != RequeueInterval {
		t.Fatalf("requeue = %v, attendu %v", requeue, RequeueInterval)
	}
	c := condOf(vc, v1alpha1.CondArgoCDReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "KeycloakClientFailed" {
		t.Fatalf("condition ArgoCDReady = %+v", c)
	}
}

// --- Rancher -------------------------------------------------------------

// Rancher désactivé pour cette cell : aucun appairage tenté.
func TestReconcileRancherPairing_SkipsWhenDisabled(t *testing.T) {
	ops := &fakeIntegrationOps{rancherStatus: service.RancherStatus{Enabled: false}}
	r := &VClusterReconciler{Cell: "preprod"}
	vc := newVCluster("rancher-off", nil)

	requeue, err := r.reconcileRancherPairing(context.Background(), ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != 0 {
		t.Fatalf("requeue = %v, attendu 0", requeue)
	}
	if _, _, _, _, _, pairCalls := ops.counts(); pairCalls != 0 {
		t.Fatal("PairRancher appelé alors que Rancher est désactivé pour cette cell")
	}
}

// Lecture Rancher ratée : inconnu ne veut pas dire "pas appairé" — pas
// d'appairage lancé sur une base incertaine, mais on revient vite regarder.
func TestReconcileRancherPairing_DoesNotPairOnUnknown(t *testing.T) {
	ops := &fakeIntegrationOps{rancherStatus: service.RancherStatus{Enabled: true, Unknown: true}}
	r := &VClusterReconciler{Cell: "prod"}
	vc := newVCluster("rancher-inconnu", nil)

	requeue, err := r.reconcileRancherPairing(context.Background(), ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != RequeueInterval {
		t.Fatalf("requeue = %v, attendu %v", requeue, RequeueInterval)
	}
	if _, _, _, _, _, pairCalls := ops.counts(); pairCalls != 0 {
		t.Fatal("PairRancher appelé sur un état Rancher inconnu — inconnu ne veut pas dire pas appairé")
	}
}

// Déjà pris en charge (appairé, en cours, appairage manuel, ou nettoyage en
// cours) : dans tous les cas, pas de second appairage déclenché.
func TestReconcileRancherPairing_SkipsWhenAlreadyHandled(t *testing.T) {
	cases := []struct {
		nom string
		st  service.RancherStatus
	}{
		{"déjà appairé", service.RancherStatus{Enabled: true, Paired: true}},
		{"appairage en cours", service.RancherStatus{Enabled: true, Pairing: true}},
		{"appairage manuel", service.RancherStatus{Enabled: true, ManuallyPaired: true}},
		{"nettoyage en cours", service.RancherStatus{Enabled: true, Cleaning: true}},
	}
	for _, tt := range cases {
		t.Run(tt.nom, func(t *testing.T) {
			ops := &fakeIntegrationOps{rancherStatus: tt.st}
			r := &VClusterReconciler{Cell: "prod"}
			vc := newVCluster("rancher-pris-en-charge", nil)

			if _, err := r.reconcileRancherPairing(context.Background(), ops, vc); err != nil {
				t.Fatalf("erreur inattendue : %v", err)
			}
			if _, _, _, _, _, pairCalls := ops.counts(); pairCalls != 0 {
				t.Fatalf("PairRancher appelé alors que l'état était déjà %q", tt.nom)
			}
		})
	}
}

// Absent de Rancher, activé pour la cell : l'opérateur appaire — c'est la
// moitié manquante de l'asymétrie avec UnpairForDeletion, déjà appelé par le
// finalizer.
func TestReconcileRancherPairing_PairsWhenAbsent(t *testing.T) {
	ops := &fakeIntegrationOps{rancherStatus: service.RancherStatus{Enabled: true}}
	r := &VClusterReconciler{Cell: "prod"}
	vc := newVCluster("rancher-absent", nil)

	requeue, err := r.reconcileRancherPairing(context.Background(), ops, vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != RequeueInterval {
		t.Fatalf("requeue = %v, attendu %v", requeue, RequeueInterval)
	}
	if _, _, _, _, _, pairCalls := ops.counts(); pairCalls != 1 {
		t.Fatalf("PairRancher appelé %d fois, attendu 1", pairCalls)
	}
	if got := ops.pairEnvsSeen; len(got) != 1 || got[0] != "prod" {
		t.Fatalf("cell passée à PairRancher = %v, attendu [prod]", got)
	}
	// L'opérateur agit en tant que lui-même, pas au nom de l'utilisateur qui a
	// déclenché le reconcile (il n'y en a pas) — même acteur que le finalizer
	// utilise pour UnpairForDeletion.
	if got := ops.pairActorsSeen; len(got) != 1 || got[0] != SystemActor {
		t.Fatalf("acteur passé à PairRancher = %v, attendu [%v]", got, SystemActor)
	}
	// Aucune condition RancherPaired écrite ici : elle appartient à l'étape
	// d'observation, qui a le dernier mot sur ce que Rancher répond vraiment.
	if c := condOf(vc, v1alpha1.CondRancherPaired); c != nil {
		t.Fatalf("condition RancherPaired écrite par reconcileIntegrations : %+v — elle appartient à l'observation", c)
	}
}

// Un échec d'appairage doit remonter pour être réessayé.
func TestReconcileRancherPairing_FailureIsRetried(t *testing.T) {
	ops := &fakeIntegrationOps{
		rancherStatus: service.RancherStatus{Enabled: true},
		pairErr:       errors.New("rancher: import failed"),
	}
	r := &VClusterReconciler{Cell: "prod"}
	vc := newVCluster("rancher-echec", nil)

	_, err := r.reconcileRancherPairing(context.Background(), ops, vc)
	if err == nil {
		t.Fatal("l'échec d'appairage n'a pas été remonté")
	}
}

// --- reconcileIntegrations (orchestration) ------------------------------

// Les trois étapes sont indépendantes : l'échec de l'une n'empêche pas les
// deux autres de tourner, et leurs erreurs sont toutes visibles.
func TestReconcileIntegrations_StepsAreIndependent(t *testing.T) {
	ops := &fakeIntegrationOps{
		vaultExistsErr: errors.New("vault down"),
		keycloakErr:    errors.New("keycloak down"),
		rancherStatus:  service.RancherStatus{Enabled: true}, // celle-ci réussit
	}
	r := &VClusterReconciler{Ops: ops, Cell: "prod"}
	vc := newVCluster("integrations-partial", func(v *v1alpha1.VCluster) {
		v.Spec.ArgoCD = &v1alpha1.ArgoCDSpec{Enabled: true}
	})

	requeue, err := r.reconcileIntegrations(context.Background(), vc)
	if err == nil {
		t.Fatal("les deux échecs auraient dû être remontés")
	}
	if !strings.Contains(err.Error(), "vault down") || !strings.Contains(err.Error(), "keycloak down") {
		t.Fatalf("errors.Join n'a pas gardé les deux échecs : %v", err)
	}
	if requeue != RequeueInterval {
		t.Fatalf("requeue = %v, attendu %v (le plus court des trois)", requeue, RequeueInterval)
	}
	// Rancher, qui a réussi, a bien tourné malgré les deux échecs voisins.
	if _, _, _, _, _, pairCalls := ops.counts(); pairCalls != 1 {
		t.Fatal("Rancher n'a pas tourné alors que Vault et Keycloak échouaient à côté")
	}
	if c := condOf(vc, v1alpha1.CondVaultConfigured); c == nil || c.Status != metav1.ConditionUnknown {
		t.Fatalf("condition VaultConfigured = %+v", c)
	}
	if c := condOf(vc, v1alpha1.CondArgoCDReady); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("condition ArgoCDReady = %+v", c)
	}
}

// Le délai retenu est le plus court des trois, jamais le plus long : une
// intégration qui progresse ne doit pas attendre le rythme d'une autre qui a
// déjà fini.
func TestReconcileIntegrations_KeepsTheShortestRequeue(t *testing.T) {
	ops := &fakeIntegrationOps{
		vaultExists:   false,
		vaultReady:    false,                                              // Vault : requeue RequeueInterval (attend vault-webhook)
		rancherStatus: service.RancherStatus{Enabled: true, Paired: true}, // Rancher : requeue 0
	}
	r := &VClusterReconciler{Ops: ops, Cell: "prod"}
	// ArgoCD désactivé : Keycloak ne contribue aucun requeue non plus.
	vc := newVCluster("integrations-vault-seul-en-attente", nil)

	requeue, err := r.reconcileIntegrations(context.Background(), vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != RequeueInterval {
		t.Fatalf("requeue = %v, attendu %v", requeue, RequeueInterval)
	}
}

// Un faux de test qui n'implémente pas VClusterIntegrationOps ne doit ni
// planter le reconcile, ni écrire de condition : une condition absente est
// déjà traitée comme « cette étape n'a pas encore tourné » par
// aggregateVClusterStatus (même principe que ArgoCDReady), donc inventer un
// Unknown ici bloquerait Ready pour tous les faux voisins qui ne portent pas
// ce seam.
func TestReconcileIntegrations_DegradesSilentlyWhenSeamMissing(t *testing.T) {
	ops := &fakeVClusterOps{} // n'implémente que Suspend/Resume
	r := &VClusterReconciler{Ops: ops, Cell: "prod"}
	vc := newVCluster("sans-seam", nil)

	requeue, err := r.reconcileIntegrations(context.Background(), vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if requeue != 0 {
		t.Fatalf("requeue = %v, attendu 0", requeue)
	}
	if len(vc.Status.Conditions) != 0 {
		t.Fatalf("conditions écrites alors que le seam est absent : %+v", vc.Status.Conditions)
	}
}

// --- Bout-en-bout : le câblage dans reconcileAll (vcluster_controller.go) --
//
// reconcileAll (pas ce fichier) enchaîne provisioning → intégrations →
// observation et fusionne les deux délais de requeue. Ce test ne vérifie pas
// ma logique — déjà couverte plus haut en isolation — il vérifie que ce que je
// retourne traverse bien ce câblage-là jusqu'au Reconcile() final.
//
// fakeEndToEndOps n'est délibérément pas composé à partir de fakeProvisioner
// ou fakeIntegrationOps : les deux embarquent déjà fakeVClusterOps, et les
// embarquer ensemble ferait disparaître Suspend/Resume du jeu de méthodes par
// ambiguïté au lieu de faire échouer la compilation — exactement le piège
// silencieux que ce chantier a été prévenu d'éviter.
type fakeEndToEndOps struct {
	fakeVClusterOps
	vaultReady bool
}

func (f *fakeEndToEndOps) RenderVClusterSubstitutions(_ *models.CreateRequest, _, _ string) ([]*unstructured.Unstructured, error) {
	return nil, nil
}

func (f *fakeEndToEndOps) ObserveVCluster(context.Context, string, string) service.VClusterObservation {
	return healthyObservation()
}

func (f *fakeEndToEndOps) VaultAuthConfigured(context.Context, string, string) (bool, error) {
	return false, nil
}
func (f *fakeEndToEndOps) VaultWebhookReady(context.Context, string, string) (bool, error) {
	return f.vaultReady, nil
}
func (f *fakeEndToEndOps) ConfigureVaultAuth(context.Context, string, string) error { return nil }
func (f *fakeEndToEndOps) EnsureKeycloakClient(string, string) error                { return nil }
func (f *fakeEndToEndOps) GetRancherStatus(context.Context, string, string) service.RancherStatus {
	return service.RancherStatus{Enabled: true, Paired: true}
}
func (f *fakeEndToEndOps) PairRancher(context.Context, models.Actor, string, string) (service.RancherStatus, error) {
	return service.RancherStatus{}, nil
}

var (
	_ VClusterProvisioner    = (*fakeEndToEndOps)(nil)
	_ VClusterObserver       = (*fakeEndToEndOps)(nil)
	_ VClusterIntegrationOps = (*fakeEndToEndOps)(nil)
)

// Vault qui attend son webhook (requeue 10s) doit imposer son rythme au
// Reconcile() complet même si l'observation, elle, serait satisfaite d'attendre
// cinq minutes une fois Ready — mais ici Ready ne peut pas être atteint tant
// que VaultConfigured est à False, donc les deux bras se retrouvent sur le
// rythme court : c'est la fusion qui compte, pas la valeur en elle-même.
func TestEndToEnd_IntegrationsRequeueSurvivesThroughReconcile(t *testing.T) {
	ctx := context.Background()
	ops := &fakeEndToEndOps{vaultReady: false}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newProvisioningVCluster(t, ctx, "e2e-vault-attend", v1alpha1.VClusterSpec{})
	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != RequeueInterval {
		t.Fatalf("RequeueAfter = %v, attendu %v (le délai Vault doit traverser jusqu'au Reconcile)", res.RequeueAfter, RequeueInterval)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondVaultConfigured, metav1.ConditionFalse, "WaitingForWebhook")
	// Rancher a bien tourné dans la même passe, sans attendre Vault : les trois
	// intégrations sont indépendantes.
	requireVCCond(t, got, v1alpha1.CondRancherPaired, metav1.ConditionTrue, "Paired")
}

// Une fois Vault configuré, plus rien n'impose le rythme court : le Reconcile()
// retombe sur le rythme d'observation (settled).
func TestEndToEnd_SettlesOnObservationRequeueOnceVaultIsDone(t *testing.T) {
	ctx := context.Background()
	ops := &fakeEndToEndOps{vaultReady: true}
	r := &VClusterReconciler{Client: k8sClient, Ops: ops, Cell: "preprod", Namespace: "default"}

	vc := newProvisioningVCluster(t, ctx, "e2e-vault-fait", v1alpha1.VClusterSpec{})
	res, err := r.Reconcile(ctx, vcReq(vc))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != ObserveIntervalSettled {
		t.Fatalf("RequeueAfter = %v, attendu %v", res.RequeueAfter, ObserveIntervalSettled)
	}

	got := fetchVCluster(t, ctx, vc)
	requireVCCond(t, got, v1alpha1.CondVaultConfigured, metav1.ConditionTrue, "Configured")
	requireVCCond(t, got, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "")
}

// --- minPositiveDuration ---------------------------------------------------
//
// Testé à part avec deux magnitudes réellement différentes : dans les tests
// ci-dessus, les trois étapes ne retournent jamais que 0 ou RequeueInterval,
// donc une inversion du comparateur dans merge() y passerait inaperçue (la
// branche "requeue == 0" masque tout). Ici, aucune échappatoire.
func TestMinPositiveDuration(t *testing.T) {
	tests := []struct {
		nom  string
		a, b time.Duration
		want time.Duration
	}{
		{"les deux à zéro", 0, 0, 0},
		{"a nul, b positif", 0, 30 * time.Second, 30 * time.Second},
		{"a positif, b nul", 10 * time.Second, 0, 10 * time.Second},
		{"a plus court", 10 * time.Second, 30 * time.Second, 10 * time.Second},
		{"b plus court", 5 * time.Minute, 10 * time.Second, 10 * time.Second},
		{"égaux", 10 * time.Second, 10 * time.Second, 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.nom, func(t *testing.T) {
			if got := minPositiveDuration(tt.a, tt.b); got != tt.want {
				t.Fatalf("minPositiveDuration(%v, %v) = %v, attendu %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
