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

// budgetOps donne au reconciler de test un résolveur de quotas réel — celui du
// générateur. Sans lui, ces tests vérifieraient une règle de quota que ni le
// provisionnement ni la production n'appliquent, ce qui est exactement la
// divergence que N2 a révélée.
func budgetOps() *fakeProvisioner { return newFakeProvisioner() }

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
	// BudgetOps non nil : c'est ce qui dit « le contrôle est branché ». Sans lui,
	// il n'y a pas de contrôle du tout et la question du plafond ne se pose pas.
	r := &VClusterReconciler{Cell: "cell1", Ops: budgetOps(), BudgetOps: &fakeBudgetReader{}} // Budget vide
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

// `enabled: false` est le SEUL opt-out : c'est un geste explicite, lisible en
// revue, et rien n'est alors imputé à la cell.
//
// Un bloc `quotas` ABSENT, lui, est imputé — le générateur y applique ses valeurs
// par défaut, donc un ResourceQuota est bel et bien écrit. C'était le trou N2 :
// omettre trois lignes du CR suffisait à obtenir un quota que le plafond de la
// cell ne comptait jamais, et qui comptait ensuite contre les vclusters suivants.
func TestBudgetIgnoresOnlyAnExplicitOptOut(t *testing.T) {
	r := &VClusterReconciler{Cell: "cell1", Ops: budgetOps(), BudgetOps: &fakeBudgetReader{}}
	vc := newVCluster("sans-opt-in", func(v *v1alpha1.VCluster) {
		v.Spec.Quotas = &v1alpha1.QuotaSpec{Enabled: false}
	})

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
				Ops:       budgetOps(),
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
		Ops:       budgetOps(),
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
		Ops:       budgetOps(),
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
	r := &VClusterReconciler{Cell: "cell1", Ops: budgetOps(), Budget: BudgetLimits{CPU: "32"}, BudgetOps: reader}
	vc := newVCluster("deja-provisionne", withQuotas("8", "", ""))

	if _, err := r.checkResourceBudget(context.Background(), vc); err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if reader.excluding != "deja-provisionne" {
		t.Fatalf("exclusion demandée = %q, attendu \"deja-provisionne\" : son quota est compté deux fois", reader.excluding)
	}
}

// Un bloc `quotas` absent est imputé au budget, aux valeurs par défaut du
// générateur. C'est le cœur de N2 : le provisionnement les écrit — le commentaire
// de QuotaSpec le revendique, « forgetting the block is the safe outcome » — et le
// budget ne les comptait pas. Un CR de trois lignes de moins passait donc devant le
// contrôle sans être vu, puis consommait le plafond des vclusters suivants.
func TestBudgetBillsAnAbsentQuotaBlockAtTheGeneratorDefaults(t *testing.T) {
	reader := &fakeBudgetReader{}
	r := &VClusterReconciler{
		Cell: "cell1", Ops: budgetOps(),
		// Le défaut du générateur est 8 CPU. 28 déjà alloués + 8 > 32.
		Budget:    BudgetLimits{CPU: "32"},
		BudgetOps: reader,
	}
	reader.used = models.BudgetUsage{CPU: qty("28")}

	vc := newVCluster("quota-implicite", nil) // aucun bloc quotas
	ok, err := r.checkResourceBudget(context.Background(), vc)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if ok {
		t.Fatal("accepté : le quota par défaut n'a pas été imputé, donc omettre le bloc " +
			"`quotas` contourne le contrôle de budget")
	}
	if c := budgetCond(vc); c == nil || c.Reason != "BudgetExceeded" {
		t.Fatalf("condition = %+v, attendu BudgetExceeded", c)
	}
}

// Sans résolveur de quotas, le contrôle refuse au lieu de laisser passer.
//
// « Je ne peux pas savoir ce qui sera écrit » n'est pas « il n'y a rien à
// imputer ». C'est la même règle que §5.3 sur le plafond absent, et pour la même
// raison : une mauvaise configuration doit se voir tout de suite plutôt que
// d'ouvrir un trou que personne ne remarque. Laisser passer ici rouvrirait N2 par
// une autre porte — un quota provisionné et jamais compté.
func TestBudgetRefusesWhenItCannotResolveTheQuotas(t *testing.T) {
	r := &VClusterReconciler{
		Cell: "cell1",
		// Satisfait VClusterOps mais PAS QuotaResolver.
		Ops:       &fakeVClusterOps{},
		Budget:    BudgetLimits{CPU: "32"},
		BudgetOps: &fakeBudgetReader{},
	}
	vc := newVCluster("sans-resolveur", withQuotas("8", "", ""))

	ok, err := r.checkResourceBudget(context.Background(), vc)
	if err == nil {
		t.Fatal("aucune erreur : le contrôle a conclu sans savoir quel quota serait écrit")
	}
	if ok {
		t.Fatal("autorisé sans avoir pu résoudre le quota effectif")
	}
	if c := budgetCond(vc); c != nil && c.Reason == "WithinBudget" {
		t.Fatal("rapporté comme tenant dans le plafond alors que le quota est inconnu")
	}
}
