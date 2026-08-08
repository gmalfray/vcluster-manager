package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// VClusterIntegrationOps est la tranche du service que ce chantier consomme :
// Vault, Keycloak, Rancher — ce qui vit hors du cluster hôte et que le CR doit
// piloter. Déclarée ici, comme VClusterOps/VClusterObserver/VClusterProvisioner/
// VClusterDeletionOps avant elle, et fusionnée avec elles dans le type de
// r.Ops (VClusterServiceOps, vcluster_controller.go) : reconcileIntegrations
// ci-dessous lit directement r.Ops.
type VClusterIntegrationOps interface {
	// Vault : chemin d'auth kubernetes-vcluster-<nom>-<cell>.
	VaultAuthConfigured(ctx context.Context, name, env string) (bool, error)
	VaultWebhookReady(ctx context.Context, name, env string) (bool, error)
	ConfigureVaultAuth(ctx context.Context, name, env string) error

	// Keycloak : client OIDC ArgoCD.
	EnsureKeycloakClient(name, env string) error

	// Rancher : GetRancherStatus est la même lecture que l'étape d'observation
	// utilise déjà ; PairRancher est le geste qui manquait côté opérateur.
	GetRancherStatus(ctx context.Context, name, env string) service.RancherStatus
	PairRancher(ctx context.Context, actor models.Actor, name, env string) (service.RancherStatus, error)
}

var _ VClusterIntegrationOps = (*service.Service)(nil)

// reconcileIntegrations configure ce qui vit hors du cluster hôte et que le CR
// doit piloter : le backend d'authentification Vault, le client OIDC
// Keycloak, l'appairage Rancher.
//
// C'est le remplacement de la map mémoire `vaultStates` des handlers et de sa
// goroutine (`setupVaultAuthWhenReady`) : un état de configuration qui vit
// dans le processus disparaît à chaque redémarrage, d'où le rattrapage au
// démarrage qui rescanne tous les vclusters. Sur le status du CR, il survit
// comme le reste — et la question posée à chaque passage est toujours celle
// que `startVaultReconciler` posait au démarrage : « le système externe
// connaît-il déjà cet état ? », jamais un flag qu'on aurait écrit soi-même.
//
// Les trois étapes sont indépendantes (crd-vcluster.md §4.1 étape 4) : l'échec
// de l'une n'empêche pas les autres de tourner, et chacune porte sa propre
// condition. Les erreurs sont jointes, le délai de re-scrutation retenu est le
// plus court des trois.
func (r *VClusterReconciler) reconcileIntegrations(ctx context.Context, vc *v1alpha1.VCluster) (time.Duration, error) {
	var (
		errs    []error
		requeue time.Duration
	)
	merge := func(d time.Duration, err error) {
		if err != nil {
			errs = append(errs, err)
		}
		requeue = minPositiveDuration(requeue, d)
	}

	merge(r.reconcileVault(ctx, r.Ops, vc))
	merge(r.reconcileKeycloak(r.Ops, vc))
	merge(r.reconcileRancherPairing(ctx, r.Ops, vc))

	return requeue, errors.Join(errs...)
}

// reconcileVault configure le backend d'authentification Kubernetes de Vault
// pour ce vcluster (crd-vcluster.md §3.2). Il écrit `status.vault` et la
// condition `VaultConfigured` — les deux étaient déclarées dans la CRD sans
// jamais être écrites, l'état vivant à la place dans la map mémoire des
// handlers.
//
// Idempotent par observation : la première question est « Vault connaît-il
// déjà ce chemin ? », pas « ai-je déjà tourné ». Une fois la réponse oui, plus
// rien n'est refait — ni token régénéré, ni backend reconfiguré — tant que
// Vault ne dit pas le contraire à un passage suivant.
func (r *VClusterReconciler) reconcileVault(ctx context.Context, ops VClusterIntegrationOps, vc *v1alpha1.VCluster) (time.Duration, error) {
	exists, err := ops.VaultAuthConfigured(ctx, vc.Name, r.Cell)
	switch {
	case errors.Is(err, service.ErrVaultNotConfigured):
		// Absence de configuration, pas une panne : aucun client Vault n'a été
		// câblé sur cet opérateur. Pas d'erreur à faire réessayer en boucle.
		setVClusterCond(vc, v1alpha1.CondVaultConfigured, metav1.ConditionUnknown, "VaultNotConfigured",
			"aucun client Vault configuré sur cet opérateur : rien à vérifier")
		return 0, nil
	case err != nil:
		setVClusterCond(vc, v1alpha1.CondVaultConfigured, metav1.ConditionUnknown, "VaultUnreachable",
			"lecture du backend Vault impossible : "+err.Error())
		return RequeueInterval, fmt.Errorf("lecture du backend Vault : %w", err)
	case exists:
		setVClusterCond(vc, v1alpha1.CondVaultConfigured, metav1.ConditionTrue, "Configured",
			"backend Vault déjà configuré pour ce vcluster")
		vc.Status.Vault = &v1alpha1.VaultStatus{Status: "done"}
		return 0, nil
	}

	ready, err := ops.VaultWebhookReady(ctx, vc.Name, r.Cell)
	if err != nil {
		setVClusterCond(vc, v1alpha1.CondVaultConfigured, metav1.ConditionUnknown, "WebhookUnreadable",
			"lecture de vault-webhook impossible : "+err.Error())
		vc.Status.Vault = &v1alpha1.VaultStatus{Status: "waiting", Message: err.Error()}
		return RequeueInterval, fmt.Errorf("lecture de vault-webhook : %w", err)
	}
	if !ready {
		setVClusterCond(vc, v1alpha1.CondVaultConfigured, metav1.ConditionFalse, "WaitingForWebhook",
			"vault-webhook n'est pas encore prêt dans le vcluster")
		vc.Status.Vault = &v1alpha1.VaultStatus{Status: "waiting"}
		return RequeueInterval, nil
	}

	if err := ops.ConfigureVaultAuth(ctx, vc.Name, r.Cell); err != nil {
		setVClusterCond(vc, v1alpha1.CondVaultConfigured, metav1.ConditionFalse, "SetupFailed", err.Error())
		vc.Status.Vault = &v1alpha1.VaultStatus{Status: "error", Message: err.Error()}
		return RequeueInterval, fmt.Errorf("configuration Vault : %w", err)
	}

	setVClusterCond(vc, v1alpha1.CondVaultConfigured, metav1.ConditionTrue, "Configured", "backend Vault configuré")
	vc.Status.Vault = &v1alpha1.VaultStatus{Status: "done"}
	return 0, nil
}

// reconcileKeycloak crée le client OIDC ArgoCD du vcluster quand
// `spec.argoCD.enabled` (crd-vcluster.md §3.2). C'est la moitié manquante de
// l'asymétrie : `DeleteArgoCDClients` est déjà appelé par le finalizer, mais
// rien ne créait le client côté opérateur — il ne l'était que par
// `service.Create`, le chemin monolithe.
//
// N'écrit que ce que ce pas vérifie réellement : la condition ArgoCDReady est
// documentée (crd-vcluster.md §3.3) comme l'agrégat de Keycloak + GitLab +
// Kustomization ArgoCD, mais seul le volet Keycloak est couvert ici — le dépôt
// GitLab et la santé de la Kustomization ArgoCD restent un trou de couverture,
// signalé dans le message plutôt que caché derrière un True optimiste.
func (r *VClusterReconciler) reconcileKeycloak(ops VClusterIntegrationOps, vc *v1alpha1.VCluster) (time.Duration, error) {
	if vc.Spec.ArgoCD == nil || !vc.Spec.ArgoCD.Enabled {
		setVClusterCond(vc, v1alpha1.CondArgoCDReady, metav1.ConditionFalse, "ArgoCDDisabled",
			"spec.argoCD.enabled est à false : pas de client OIDC à créer")
		return 0, nil
	}

	err := ops.EnsureKeycloakClient(vc.Name, r.Cell)
	switch {
	case errors.Is(err, service.ErrKeycloakNotConfigured):
		setVClusterCond(vc, v1alpha1.CondArgoCDReady, metav1.ConditionUnknown, "KeycloakNotConfigured",
			"aucun client Keycloak configuré sur cet opérateur : le client OIDC n'a pas pu être vérifié")
		return 0, nil
	case err != nil:
		setVClusterCond(vc, v1alpha1.CondArgoCDReady, metav1.ConditionFalse, "KeycloakClientFailed", err.Error())
		return RequeueInterval, fmt.Errorf("client OIDC Keycloak : %w", err)
	}

	setVClusterCond(vc, v1alpha1.CondArgoCDReady, metav1.ConditionTrue, "KeycloakClientReady",
		"client OIDC ArgoCD présent dans Keycloak — ce volet seulement : le dépôt GitLab et la Kustomization "+
			"ArgoCD ne sont pas encore vérifiés par cette condition")
	return 0, nil
}

// reconcileRancherPairing appaire le vcluster dans Rancher quand la cell l'a
// activé et qu'il n'y est pas déjà connu (crd-vcluster.md §3.2). C'est la
// moitié manquante de l'asymétrie : `UnpairForDeletion` est déjà appelé par le
// finalizer, `PairRancher` ne l'était jamais côté opérateur.
//
// N'écrit aucune condition : `RancherPaired` est déjà observée et écrite par
// l'étape de status juste après (vcluster_status.go), qui a le dernier mot
// sur ce que Rancher répond réellement. Écrire ici referait ce que cette étape
// fait déjà, avec l'état du passage précédent en plus.
func (r *VClusterReconciler) reconcileRancherPairing(ctx context.Context, ops VClusterIntegrationOps, vc *v1alpha1.VCluster) (time.Duration, error) {
	st := ops.GetRancherStatus(ctx, vc.Name, r.Cell)

	if !st.Enabled {
		// Rancher n'est pas activé pour cette cell : rien à appairer.
		return 0, nil
	}
	if st.Unknown {
		// Inconnu ≠ faux : une lecture ratée n'autorise pas à conclure « pas
		// appairé », donc pas d'appairage lancé sur une base incertaine.
		return RequeueInterval, nil
	}
	if st.Paired || st.Pairing || st.ManuallyPaired || st.Cleaning {
		// Déjà pris en charge, par nous ou à la main : rien à faire.
		return 0, nil
	}

	// Ni connu, ni en cours : PairRancher revérifie lui-même
	// (FindClusterByName, HasRancherAgents) avant d'agir, donc rejouer ce pas à
	// chaque passage ne double pas l'appairage — dès qu'il démarre, l'état
	// bascule sur Pairing et ce bloc n'est plus atteint au passage suivant.
	if _, err := ops.PairRancher(ctx, SystemActor, vc.Name, r.Cell); err != nil {
		return RequeueInterval, fmt.Errorf("appairage Rancher : %w", err)
	}
	return RequeueInterval, nil
}

// minPositiveDuration renvoie le plus court des deux délais. Zéro veut dire
// « pas d'avis » et perd toujours face à une valeur positive — c'est ce qui
// permet à merge() de partir de zéro sans qu'une première étape muette
// n'écrase le délai d'une étape qui, elle, a quelque chose à surveiller.
func minPositiveDuration(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}
