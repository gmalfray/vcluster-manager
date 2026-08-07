package controller

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

type fakeBudgetReader struct {
	used      models.BudgetUsage
	err       error
	excluding string
}

func (f *fakeBudgetReader) SumVClusterQuotas(_ context.Context, _, excluding string) (models.BudgetUsage, error) {
	f.excluding = excluding
	return f.used, f.err
}

func qty(s string) resource.Quantity { return resource.MustParse(s) }

func withQuotas(cpu, mem, sto string) func(*v1alpha1.VCluster) {
	return func(v *v1alpha1.VCluster) {
		v.Spec.Quotas = &v1alpha1.QuotaSpec{Enabled: true, CPU: cpu, Memory: mem, Storage: sto}
	}
}

func budgetCond(vc *v1alpha1.VCluster) *metav1.Condition {
	for i := range vc.Status.Conditions {
		if vc.Status.Conditions[i].Type == v1alpha1.CondBudgetOK {
			return &vc.Status.Conditions[i]
		}
	}
	return nil
}

// §5.3 — la règle qui compte. Un opérateur sans plafond configuré est
// exactement le moment où le contrôle devrait le plus s'appliquer ; laisser
// passer y viderait le garde-fou de son sens.
func TestBudgetRefusesWhenNoCeilingConfigured(t *testing.T) {
	r := &VClusterReconciler{Cell: "cell1"} // Budget vide
	vc := newVCluster("sans-plafond", withQuotas("8", "32Gi", "500Gi"))

	ok, err := r.checkResourceBudget(context.Background(), vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if ok {
		t.Fatal("création autorisée sans plafond configuré — le contrôle ne sert alors à rien")
	}
	c := budgetCond(vc)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NoBudgetConfigured" {
		t.Fatalf("condition BudgetOK = %+v", c)
	}
	if vc.Status.Phase != v1alpha1.VClusterPhaseFailed {
		t.Fatalf("phase = %q, attendu Failed", vc.Status.Phase)
	}
}

// Sans quota demandé, rien n'est imputé à la cell : le plafond ne s'applique pas.
// Sinon un vcluster sans quota deviendrait impossible à créer sur un opérateur
// non configuré, ce qui n'a rien à voir avec le budget.
func TestBudgetIgnoresVClustersWithoutQuotas(t *testing.T) {
	r := &VClusterReconciler{Cell: "cell1"}
	vc := newVCluster("sans-quota", nil)

	ok, err := r.checkResourceBudget(context.Background(), vc)
	if err != nil || !ok {
		t.Fatalf("refusé alors qu'aucun quota n'est demandé (ok=%v err=%v)", ok, err)
	}
	if c := budgetCond(vc); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition BudgetOK = %+v", c)
	}
}

func TestBudgetComparesAgainstWhatTheCellAlreadyHandedOut(t *testing.T) {
	tests := []struct {
		nom      string
		deja     models.BudgetUsage
		demande  [3]string
		plafond  BudgetLimits
		autorise bool
	}{
		{
			nom:      "tient dans le plafond",
			deja:     models.BudgetUsage{CPU: qty("10"), Memory: qty("40Gi"), Storage: qty("500Gi")},
			demande:  [3]string{"8", "32Gi", "200Gi"},
			plafond:  BudgetLimits{CPU: "32", Memory: "128Gi", Storage: "2Ti"},
			autorise: true,
		},
		{
			nom:      "dépasse sur le CPU seulement",
			deja:     models.BudgetUsage{CPU: qty("30"), Memory: qty("10Gi"), Storage: qty("100Gi")},
			demande:  [3]string{"8", "8Gi", "100Gi"},
			plafond:  BudgetLimits{CPU: "32", Memory: "128Gi", Storage: "2Ti"},
			autorise: false,
		},
		{
			nom:      "pile au plafond passe",
			deja:     models.BudgetUsage{CPU: qty("24")},
			demande:  [3]string{"8", "", ""},
			plafond:  BudgetLimits{CPU: "32"},
			autorise: true,
		},
		{
			nom:      "une dimension non plafonnée n'est pas contrainte",
			deja:     models.BudgetUsage{Storage: qty("10Ti")},
			demande:  [3]string{"1", "", "5Ti"},
			plafond:  BudgetLimits{CPU: "32"}, // pas de plafond stockage
			autorise: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.nom, func(t *testing.T) {
			r := &VClusterReconciler{
				Cell:      "cell1",
				Budget:    tt.plafond,
				BudgetOps: &fakeBudgetReader{used: tt.deja},
			}
			vc := newVCluster("budget", withQuotas(tt.demande[0], tt.demande[1], tt.demande[2]))

			ok, err := r.checkResourceBudget(context.Background(), vc)
			if err != nil {
				t.Fatalf("erreur inattendue : %v", err)
			}
			if ok != tt.autorise {
				c := budgetCond(vc)
				t.Fatalf("autorisé = %v, attendu %v (condition : %+v)", ok, tt.autorise, c)
			}
		})
	}
}

// Une lecture ratée n'est pas un dépassement. Confondre « je ne sais pas » et
// « ça dépasse » bloquerait toute création dès que l'API a un hoquet.
func TestBudgetDoesNotTreatAFailedReadAsExceeded(t *testing.T) {
	r := &VClusterReconciler{
		Cell:      "cell1",
		Budget:    BudgetLimits{CPU: "32"},
		BudgetOps: &fakeBudgetReader{err: errors.New("apiserver injoignable")},
	}
	vc := newVCluster("lecture-ratee", withQuotas("8", "", ""))

	ok, err := r.checkResourceBudget(context.Background(), vc)
	if err == nil {
		t.Fatal("l'échec de lecture n'a pas été remonté : rien ne sera réessayé")
	}
	if ok {
		t.Fatal("autorisé malgré une lecture ratée")
	}
	if c := budgetCond(vc); c != nil && c.Reason == "BudgetExceeded" {
		t.Fatal("une lecture ratée a été rapportée comme un dépassement")
	}
}

// Un quota illisible doit être refusé avec un message qui le nomme, pas faire
// planter la réconciliation.
func TestBudgetRejectsUnparseableQuantities(t *testing.T) {
	r := &VClusterReconciler{
		Cell:      "cell1",
		Budget:    BudgetLimits{CPU: "32"},
		BudgetOps: &fakeBudgetReader{},
	}
	vc := newVCluster("quota-illisible", withQuotas("beaucoup", "", ""))

	ok, err := r.checkResourceBudget(context.Background(), vc)
	if err != nil {
		t.Fatalf("devrait être un refus, pas une erreur de réconciliation : %v", err)
	}
	if ok {
		t.Fatal("quota illisible accepté")
	}
	c := budgetCond(vc)
	if c == nil || c.Reason != "InvalidQuantity" {
		t.Fatalf("condition = %+v", c)
	}
}

// Le vcluster réconcilié doit être RETIRÉ du total déjà alloué : sinon son
// propre quota est compté deux fois — une fois lu depuis la cell, une fois
// ajouté comme demande — et il se met à échouer sa propre vérification dès
// qu'il est provisionné, sur un reconcile qui ne change rien.
func TestBudgetExcludesTheVClusterBeingReconciled(t *testing.T) {
	reader := &fakeBudgetReader{used: models.BudgetUsage{CPU: qty("10")}}
	r := &VClusterReconciler{Cell: "cell1", Budget: BudgetLimits{CPU: "32"}, BudgetOps: reader}
	vc := newVCluster("deja-provisionne", withQuotas("8", "", ""))

	if _, err := r.checkResourceBudget(context.Background(), vc); err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if reader.excluding != "deja-provisionne" {
		t.Fatalf("exclusion demandée = %q, attendu \"deja-provisionne\" : son quota est compté deux fois", reader.excluding)
	}
}
