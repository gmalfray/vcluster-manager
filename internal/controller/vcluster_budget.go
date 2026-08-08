package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// BudgetLimits est le plafond de ressources de la cell, configuré à la main par
// l'exploitation (RESOURCE_BUDGET_CPU / _MEMORY / _STORAGE).
//
// Un plafond statique, et non la somme des `Allocatable` des nœuds : le cluster
// hôte porte aussi autre chose que des vclusters, et calculer depuis les nœuds
// reviendrait à chercher à remplir le cluster plutôt qu'à garder une marge
// délibérée (crd-vcluster.md §5.2).
type BudgetLimits struct {
	CPU     string
	Memory  string
	Storage string
}

// configured reports whether a ceiling was set at all.
func (b BudgetLimits) configured() bool {
	return b.CPU != "" || b.Memory != "" || b.Storage != ""
}

// QuotaResolver dit quel quota sera réellement écrit pour un vcluster. Déclarée
// ici, où elle est consommée ; *service.Service la satisfait.
type QuotaResolver interface {
	EffectiveQuotas(req *models.CreateRequest, env string) (cpu, mem, sto string, enabled bool, err error)
}

var _ QuotaResolver = (*service.Service)(nil)

// BudgetRetryInterval est le rythme auquel un vcluster refusé pour dépassement
// revient frapper à la porte.
//
// Sans lui, un refus ne se réveillait jamais tout seul : reconcileAll rendait
// `0, nil`, et le reconciler ne surveille que les VCluster — la suppression d'un
// VOISIN, qui est justement ce qui libère la place, ne produit aucun événement
// ici. Le scénario normal de la file d'attente (« je crée, ça ne rentre pas, je
// supprime un vieux, ça devrait repartir ») ne fonctionnait donc pas :
// crd-vcluster.md §4.1 point 2 demande explicitement ce requeue.
//
// Cinq minutes : la place se libère à l'échelle d'une suppression de vcluster,
// pas à la seconde, et un refus qui repolle vite ne libère rien plus tôt.
const BudgetRetryInterval = 5 * time.Minute

// BudgetReader reads the cell's current allocation. Declared here, where it is
// consumed; the production implementation lives in the service.
//
// The second argument is the vcluster to leave out of the total — the one being
// reconciled, whose own quota the caller is about to add back.
type BudgetReader interface {
	SumVClusterQuotas(ctx context.Context, env, excluding string) (models.BudgetUsage, error)
}

var _ BudgetReader = (*service.Service)(nil)

// checkResourceBudget refuses to provision a vcluster whose quotas would push
// the cell past its ceiling.
//
// Fait au reconcile plutôt que par un webhook d'admission (crd-vcluster.md §5.1) :
// même garde-fou visible côté Flux — la Kustomization a un health check sur la
// condition Ready — sans certificat TLS à maintenir ni service dont la panne
// bloquerait toute admission de CR, y compris des mises à jour sans rapport.
//
// Renvoie true si le provisionnement peut continuer.
func (r *VClusterReconciler) checkResourceBudget(ctx context.Context, vc *v1alpha1.VCluster) (bool, error) {
	// Le contrôle est-il branché du tout ? Cette question passe en premier.
	//
	// Sans lecteur d'allocation il n'y a pas de contrôle de budget, donc rien ne
	// justifie d'exiger de savoir quels quotas seront écrits ni qu'un plafond soit
	// configuré. Mettre cette question après les autres rendait le résolveur de
	// quotas obligatoire sur TOUS les chemins de réconciliation, y compris ceux qui
	// n'ont rien à voir avec le budget.
	//
	// En production BudgetOps est toujours renseigné (cmd/operator/main.go), donc
	// ce raccourci n'ouvre rien : il ne sert qu'aux tests qui ne portent pas sur le
	// budget.
	if r.BudgetOps == nil {
		return true, nil
	}

	cpu, mem, sto, billable, err := r.effectiveQuotas(vc)
	if err != nil {
		// Ne pas conclure sans savoir ce qui sera écrit : « je ne peux pas
		// calculer le quota » n'est pas « il n'y a rien à imputer ».
		return false, err
	}
	if !billable {
		setVClusterCond(vc, v1alpha1.CondBudgetOK, metav1.ConditionTrue, "NoQuotaRequested",
			"aucun quota ne sera écrit, rien à imputer au budget de la cell")
		return true, nil
	}

	// §5.3 — refuser, pas laisser passer.
	//
	// Un plafond absent est le moment où le contrôle devrait le plus s'appliquer
	// (opérateur mal configuré), pas celui où on l'abandonne. Le compromis est
	// assumé : une mauvaise configuration se voit immédiatement — plus aucune
	// création ne passe — donc elle se corrige vite, au lieu d'ouvrir un trou que
	// personne ne remarque jamais.
	if !r.Budget.configured() {
		setVClusterCond(vc, v1alpha1.CondBudgetOK, metav1.ConditionFalse, "NoBudgetConfigured",
			"aucun plafond de ressources configuré sur cet opérateur (RESOURCE_BUDGET_CPU/_MEMORY/_STORAGE) : "+
				"la création est refusée plutôt que laissée passer sans contrôle")
		vc.Status.Phase = v1alpha1.VClusterPhaseFailed
		return false, nil
	}

	used, err := r.BudgetOps.SumVClusterQuotas(ctx, r.Cell, vc.Name)
	if err != nil {
		// Ne pas conclure sur une lecture ratée : « je ne sais pas » n'est pas
		// « ça dépasse ». On remonte l'erreur, la réconciliation réessaiera.
		return false, fmt.Errorf("lecture des quotas de la cell : %w", err)
	}

	for _, d := range []struct {
		nom      string
		demandee string
		deja     resource.Quantity
		plafond  string
	}{
		{"CPU", cpu, used.CPU, r.Budget.CPU},
		{"mémoire", mem, used.Memory, r.Budget.Memory},
		{"stockage", sto, used.Storage, r.Budget.Storage},
	} {
		depasse, msg, err := exceedsBudget(d.nom, d.demandee, d.deja, d.plafond)
		if err != nil {
			setVClusterCond(vc, v1alpha1.CondBudgetOK, metav1.ConditionFalse, "InvalidQuantity", err.Error())
			vc.Status.Phase = v1alpha1.VClusterPhaseFailed
			return false, nil
		}
		if depasse {
			setVClusterCond(vc, v1alpha1.CondBudgetOK, metav1.ConditionFalse, "BudgetExceeded", msg)
			vc.Status.Phase = v1alpha1.VClusterPhaseFailed
			return false, nil
		}
	}

	setVClusterCond(vc, v1alpha1.CondBudgetOK, metav1.ConditionTrue, "WithinBudget",
		"les quotas demandés tiennent dans le plafond de la cell")
	return true, nil
}

// effectiveQuotas rend le quota qui sera RÉELLEMENT écrit pour ce vcluster, et
// s'il est imputable au budget de la cell.
//
// C'est le point où deux conventions opposées cohabitaient sur le même champ.
// createRequestFromCR fait `NoQuotas = Quotas != nil && !Enabled` — bloc absent
// vaut quotas ACTIFS, aux valeurs par défaut du générateur, ce que le commentaire
// de QuotaSpec revendique explicitement (« forgetting the block is the safe
// outcome »). Le budget, lui, faisait `Quotas == nil || !Enabled` : bloc absent
// valait « rien à imputer ».
//
// Résultat mesuré par la recette : QUOTA_CPU=8 / QUOTA_MEMORY=32Gi /
// QUOTA_STORAGE=500Gi provisionnés, zéro imputé. Et comme SumVClusterQuotas somme
// les ResourceQuota des namespaces vcluster-*, ce quota comptait ensuite contre
// les vclusters SUIVANTS — donc la cell dépassait silencieusement d'un tenant à
// chaque CR qui omettait trois lignes. Omettre trois lignes contournait « le
// contrôle qui compte » d'ADR-001.
//
// On impute donc le quota effectif, et non le fait que le bloc soit là.
// `enabled: false` reste le seul opt-out, parce que c'est un geste explicite et lisible en revue.
// Un quota effectif entièrement vide n'est pas imputable : aucune valeur ne sera
// écrite, il n'y a rien à compter.
func (r *VClusterReconciler) effectiveQuotas(vc *v1alpha1.VCluster) (cpu, mem, sto string, billable bool, err error) {
	cpu, mem, sto, enabled, err := r.Ops.EffectiveQuotas(createRequestFromCR(vc), r.Cell)
	if err != nil {
		return "", "", "", false, err
	}
	// `enabled: false` est le seul opt-out, et c'est un geste explicite, lisible en
	// revue. Un quota effectif entièrement vide n'est pas imputable non plus :
	// aucune valeur ne sera écrite, il n'y a rien à compter.
	if !enabled {
		return "", "", "", false, nil
	}
	return cpu, mem, sto, cpu != "" || mem != "" || sto != "", nil
}

// exceedsBudget compares "already handed out + requested" against the ceiling.
// An unset ceiling for one dimension means that dimension is not capped.
func exceedsBudget(nom, demandee string, deja resource.Quantity, plafond string) (bool, string, error) {
	if plafond == "" || demandee == "" {
		return false, "", nil
	}
	dem, err := resource.ParseQuantity(demandee)
	if err != nil {
		return false, "", fmt.Errorf("quota %s illisible (%q) : %w", nom, demandee, err)
	}
	max, err := resource.ParseQuantity(plafond)
	if err != nil {
		return false, "", fmt.Errorf("plafond %s de la cell illisible (%q) : %w", nom, plafond, err)
	}

	total := deja.DeepCopy()
	total.Add(dem)
	if total.Cmp(max) > 0 {
		return true, fmt.Sprintf(
			"budget %s dépassé sur la cell : %s déjà alloués + %s demandés = %s, plafond %s",
			nom, deja.String(), dem.String(), total.String(), max.String()), nil
	}
	return false, "", nil
}
