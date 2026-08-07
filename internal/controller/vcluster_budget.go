package controller

import (
	"context"
	"fmt"

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
	// Pas de quotas demandés : rien à imputer au budget de la cell.
	if vc.Spec.Quotas == nil || !vc.Spec.Quotas.Enabled {
		setVClusterCond(vc, v1alpha1.CondBudgetOK, metav1.ConditionTrue, "NoQuotaRequested",
			"aucun quota demandé, rien à imputer au budget de la cell")
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

	if r.BudgetOps == nil {
		return true, nil
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
		{"CPU", vc.Spec.Quotas.CPU, used.CPU, r.Budget.CPU},
		{"mémoire", vc.Spec.Quotas.Memory, used.Memory, r.Budget.Memory},
		{"stockage", vc.Spec.Quotas.Storage, used.Storage, r.Budget.Storage},
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
