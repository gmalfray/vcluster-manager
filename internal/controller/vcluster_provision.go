package controller

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// ProvisionFieldManager est le field manager sous lequel l'opérateur applique
// ce qu'il possède.
//
// Réponse à crd-vcluster.md §7, inconnue 1 (« qui gagne sur un champ que Flux et
// nous touchons tous les deux ? ») en trois temps.
//
// D'abord le ConfigMap de substitutions : il n'apparaît dans aucun manifeste
// commité, donc personne d'autre ne l'écrit. C'est tout l'intérêt de n'appliquer
// que des objets de ce genre.
//
// Le namespace, lui, a bien deux écrivains, et il faut le dire — ce commentaire
// a affirmé le contraire. Il vient aussi de `clusters/<cell>/base`, dont
// l'overlay du tenant patche le `metadata.name` : Flux l'applique donc à chaque
// réconciliation, en parallèle de nous. Ce n'est pas un contentieux pour autant.
// L'objet est nu — un nom, rien d'autre — et un nom n'est pas un champ géré mais
// l'identité de l'objet : les deux écrivains déclarent la même chose, il n'y a
// pas de valeur sur laquelle diverger. La propriété tranchée est donc :
// l'opérateur possède la CRÉATION et la SUPPRESSION de ce namespace (le
// finalizer le supprime lui-même, arbitrage N6), Flux en possède la
// réapplication tant que l'overlay du tenant est commité. La conséquence
// pratique est dans reconcileFinalTeardownAndNamespaceRemoval : un namespace qu'on supprime
// alors que la Kustomization du tenant vit encore réapparaît, et l'étape le
// rapporte au lieu de prétendre l'avoir détruit.
//
// Enfin, il reste les écrivains occasionnels — un `kubectl edit` de dépannage,
// un contrôleur tiers. Sans ForceOwnership, le premier d'entre eux prend la
// propriété d'une clé et bloque toutes les réconciliations suivantes sur un
// conflit : l'opérateur se retrouve incapable de réparer son propre objet. On
// force donc, ce qui reprend la propriété des clés que l'opérateur DÉCLARE, et
// uniquement celles-là — Server-Side Apply fusionne, il ne remplace pas, donc
// une clé ajoutée à côté survit. C'est vérifié par
// TestProvisioningOnlyOwnsWhatItDeclares.
const ProvisionFieldManager = "vcluster-manager-operator"

// VClusterProvisioner rend les objets que l'opérateur applique lui-même.
//
// Déclaré ici, là où il est consommé, comme VClusterOps et BudgetReader.
// r.Ops est typé VClusterServiceOps (vcluster_controller.go), qui embarque
// cette interface avec les cinq autres : reconcileProvisioning ci-dessous lit
// directement r.Ops, sans assertion de type.
type VClusterProvisioner interface {
	RenderVClusterSubstitutions(req *models.CreateRequest, env, k8sVersion string) ([]*unstructured.Unstructured, error)
}

var _ VClusterProvisioner = (*service.Service)(nil)

// reconcileProvisioning matérialise ce que le CR dérive, et rien d'autre
// (crd-vcluster.md §4.1 étape 3).
//
// Deux objets : le namespace du vcluster, et le ConfigMap
// vcluster-<nom>-substitutions que Flux injecte dans les templates tenant
// partagés via postBuild.substituteFrom. Pas l'arborescence entière.
//
// Le raisonnement, parce qu'il n'est pas évident : sur les 17 documents que
// produit le générateur, 7 sont des objets Kubernetes — mais 5 de ces 7 sont des
// Kustomization Flux dont le `path` pointe vers un répertoire d'overlay par
// vcluster, qui doit rester commité. Les appliquer depuis l'opérateur donnerait
// deux propriétaires (l'opérateur pour l'objet, Git pour son contenu) sans
// supprimer un seul fichier. En basculant ces Kustomization vers ./lib partagé
// + substitution, l'overlay disparaît et il ne reste qu'une chose à rendre
// depuis le CR : les valeurs. C'est ce ConfigMap.
//
// Conséquence directe sur les deux inconnues du §7 : pas de contentieux de field
// manager (inconnue 1) — voir ProvisionFieldManager, qui détaille pourquoi le
// namespace échappe à la règle sans pour autant créer de conflit — et pas de
// prune à faire (inconnue 2) puisque désactiver une option vide une clé au lieu
// de retirer un objet. Le prune du reste appartient à Flux, qui le fait déjà.
// Retourne (continuer, erreur). `continuer=false` sans erreur est un REFUS :
// rien n'a été provisionné et rien ne le sera tant que le spec n'aura pas changé.
// L'appelant doit alors s'arrêter là et agréger — même patron que
// checkResourceBudget.
//
// C'est ce que rendre `nil` ne disait pas : la réconciliation enchaînait sur
// l'observation, qui réécrivait ResourcesProvisioned depuis ce qu'elle voyait, et
// un CR refusé ressortait Ready=True. Le refus disparaissait sur le canal même
// que Flux lit pour son health check.
func (r *VClusterReconciler) reconcileProvisioning(ctx context.Context, vc *v1alpha1.VCluster) (bool, error) {
	// §4.1 étape 1. La règle CEL de la CRD refuse déjà `capi` à l'admission ;
	// cette branche couvre un CR admis avant que la règle n'existe.
	if vc.Spec.Type == v1alpha1.VClusterTypeCAPI {
		vc.Status.Phase = v1alpha1.VClusterPhaseFailed
		setVClusterCond(vc, v1alpha1.CondResourcesProvisioned, metav1.ConditionFalse, "TypeNotImplemented",
			"type: capi est réservé, Cluster API n'est pas implémenté (docs/etude-cluster-api.md) — rien n'a été provisionné")
		return false, nil
	}

	// Le nom sert de suffixe de namespace juste après. Un namespace est la seule
	// frontière entre deux tenants, donc on le valide avant la concaténation.
	// Le service revalide de son côté ; ici c'est pour en faire une condition
	// lisible plutôt qu'une erreur de réconciliation qu'on réessaierait en
	// boucle, puisque relire le même nom donnera le même refus.
	if !service.ValidName(vc.Name) {
		vc.Status.Phase = v1alpha1.VClusterPhaseFailed
		setVClusterCond(vc, v1alpha1.CondResourcesProvisioned, metav1.ConditionFalse, "InvalidName",
			fmt.Sprintf("nom de vcluster refusé (%q) : attendu [a-z][a-z0-9-]*", vc.Name))
		return false, nil
	}

	objects, err := r.Ops.RenderVClusterSubstitutions(createRequestFromCR(vc), r.Cell, vc.Spec.K8sVersion)
	if err != nil {
		return false, r.provisionFailed(ctx, vc, "RenderFailed", err)
	}

	for _, obj := range objects {
		// client.Apply comme type de patch est déprécié depuis controller-runtime
		// v0.23 au profit de Client.Apply, qui prend une ApplyConfiguration.
		if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
			client.FieldOwner(ProvisionFieldManager), client.ForceOwnership); err != nil {
			return false, r.provisionFailed(ctx, vc, "ApplyFailed",
				fmt.Errorf("application de %s %s/%s : %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err))
		}
	}

	if vc.Status.Phase == "" || vc.Status.Phase == v1alpha1.VClusterPhaseFailed {
		vc.Status.Phase = v1alpha1.VClusterPhaseProvisioning
	}
	setVClusterCond(vc, v1alpha1.CondResourcesProvisioned, metav1.ConditionTrue, "Applied",
		fmt.Sprintf("namespace et substitutions appliqués ; Flux rend les ressources tenant depuis ./lib avec ces valeurs (%d objets)", len(objects)))
	return true, nil
}

// provisionFailed enregistre pourquoi le provisionnement s'est arrêté, puis rend
// l'erreur.
//
// Il écrit le status lui-même parce que Reconcile rend la main dès que cette
// fonction échoue : son propre Status().Update ne tourne pas, et la condition
// serait perdue. Un échec que personne ne voit dans `kubectl describe` est
// exactement l'échec silencieux dont ADR-001 se méfie. À retirer le jour où
// Reconcile écrira le status sur tous les chemins.
func (r *VClusterReconciler) provisionFailed(ctx context.Context, vc *v1alpha1.VCluster, reason string, cause error) error {
	setVClusterCond(vc, v1alpha1.CondResourcesProvisioned, metav1.ConditionFalse, reason, cause.Error())
	if err := r.Status().Update(ctx, vc); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// createRequestFromCR projette le CR sur la forme que le générateur parle déjà.
//
// Une projection et rien d'autre : tout ce qui se dérive — host API, domaine,
// secret TLS, client ID ArgoCD, révision cible, pod security — reste l'affaire
// du générateur, recalculée à chaque reconcile depuis le nom et la cell
// (crd-vcluster.md §2.2). Les figer dans le CR casserait la promotion
// preprod→prod.
func createRequestFromCR(vc *v1alpha1.VCluster) *models.CreateRequest {
	req := &models.CreateRequest{Name: vc.Name}

	if a := vc.Spec.ArgoCD; a != nil && a.Enabled {
		req.ArgoCD = true
		req.ArgoCDVersion = a.Version
		req.RBACGroups = a.RBACGroups
	}

	if v := vc.Spec.Velero; v != nil && v.Enabled {
		req.VeleroEnabled = true
		req.VeleroHour = v.Hour
		// Le CR porte la forme courte ("30j"), lisible dans un diff de vingt
		// lignes ; Velero veut "720h0m0s". La conversion est au contrôleur, pas
		// au relecteur.
		req.VeleroTTL = gitops.VeleroTTLFromShort(v.TTL)
	}

	// Le CR dit les quotas au positif, le générateur au négatif. Un bloc absent
	// vaut quotas ACTIFS : oublier le bloc doit donner le cas sûr, pas un tenant
	// sans plafond.
	req.NoQuotas = vc.Spec.Quotas != nil && !vc.Spec.Quotas.Enabled
	if q := vc.Spec.Quotas; q != nil {
		req.CPU, req.Memory, req.Storage = q.CPU, q.Memory, q.Storage
	}

	if f := vc.Spec.FluxCD; f != nil && f.RepoURL != "" {
		req.FluxCDEnabled = true
		req.FluxCDRepoURL = f.RepoURL
		req.FluxCDBranch = f.Branch
		req.FluxCDPath = f.Path
	}

	return req
}
