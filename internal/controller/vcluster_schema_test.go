package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
)

// Le schéma d'un CRD n'est pas de la documentation : c'est l'API server qui
// l'applique, ou personne. Ces tests le vérifient contre un vrai apiserver
// (envtest) plutôt que de faire confiance aux marqueurs kubebuilder — une faute
// de frappe dans un marqueur ne casse rien à la compilation, elle ouvre
// simplement une porte.

func newVCluster(name string, mutate func(*v1alpha1.VCluster)) *v1alpha1.VCluster {
	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.VClusterSpec{
			Type:  v1alpha1.VClusterTypeVCluster,
			Owner: "equipe-plateforme",
		},
	}
	if mutate != nil {
		mutate(vc)
	}
	return vc
}

// type: capi est réservé. Il doit exister dans l'énumération — pour que son
// arrivée ne soit pas un changement cassant — mais être refusé tant que Cluster
// API n'est pas implémenté. Les deux moitiés comptent.
func TestSchemaRefusesReservedCAPIType(t *testing.T) {
	ctx := context.Background()
	vc := newVCluster("capi-reserve", func(v *v1alpha1.VCluster) {
		v.Spec.Type = v1alpha1.VClusterTypeCAPI
	})

	err := k8sClient.Create(ctx, vc)
	if err == nil {
		_ = k8sClient.Delete(ctx, vc)
		t.Fatal("type: capi accepté alors qu'il est réservé — quelqu'un croira pouvoir s'en servir")
	}
	if !strings.Contains(err.Error(), "réservé") {
		t.Fatalf("refusé, mais le message n'explique pas pourquoi : %v", err)
	}
}

func TestSchemaRefusesUnknownType(t *testing.T) {
	ctx := context.Background()
	vc := newVCluster("type-inconnu", func(v *v1alpha1.VCluster) {
		v.Spec.Type = v1alpha1.VClusterType("k3s")
	})
	if err := k8sClient.Create(ctx, vc); err == nil {
		_ = k8sClient.Delete(ctx, vc)
		t.Fatal("un type hors énumération a été accepté")
	}
}

// Owner est requis : sans propriétaire, ni le budget de ressources ni la revue
// de branche protégée n'ont quelqu'un à qui parler (crd-vcluster.md §2.2).
func TestSchemaRequiresOwner(t *testing.T) {
	ctx := context.Background()
	vc := newVCluster("sans-owner", func(v *v1alpha1.VCluster) { v.Spec.Owner = "" })
	if err := k8sClient.Create(ctx, vc); err == nil {
		_ = k8sClient.Delete(ctx, vc)
		t.Fatal("un vcluster sans propriétaire a été accepté")
	}
}

// Les formes courtes du CR sont contraintes, sinon la conversion faite par le
// contrôleur échouerait au runtime au lieu d'être refusée à l'écriture.
func TestSchemaConstrainsVeleroShortForms(t *testing.T) {
	tests := []struct {
		nom      string
		hour     string
		ttl      string
		accepter bool
	}{
		{"formes valides", "03:30", "30j", true},
		{"heure Velero brute refusée", "0330", "30j", false},
		{"heure hors plage refusée", "25:00", "30j", false},
		{"ttl brut Velero refusé", "03:30", "720h0m0s", false},
		{"ttl sans unité refusé", "03:30", "30", false},
	}
	for _, tt := range tests {
		t.Run(tt.nom, func(t *testing.T) {
			ctx := context.Background()
			vc := newVCluster("velero-"+strings.ReplaceAll(strings.ToLower(tt.nom), " ", "-"), func(v *v1alpha1.VCluster) {
				v.Spec.Velero = &v1alpha1.VeleroSpec{Enabled: true, Hour: tt.hour, TTL: tt.ttl}
			})
			err := k8sClient.Create(ctx, vc)
			if err == nil {
				defer func() { _ = k8sClient.Delete(ctx, vc) }()
			}
			if tt.accepter && err != nil {
				t.Fatalf("refusé alors que la forme est valide : %v", err)
			}
			if !tt.accepter && err == nil {
				t.Fatal("accepté alors que la forme est invalide")
			}
		})
	}
}

// Un CR minimal doit passer, et `type` prendre sa valeur par défaut : c'est ce
// qui permet à l'objet de rester « vingt lignes lisibles en revue » (ADR-001).
func TestSchemaAcceptsMinimalVCluster(t *testing.T) {
	ctx := context.Background()
	vc := &v1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "minimal", Namespace: "default"},
		Spec:       v1alpha1.VClusterSpec{Owner: "greg"},
	}
	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("un CR minimal a été refusé : %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, vc) }()

	if vc.Spec.Type != v1alpha1.VClusterTypeVCluster {
		t.Fatalf("type = %q, attendu la valeur par défaut vcluster", vc.Spec.Type)
	}
}

// Le status doit être une sous-ressource : sans ça, n'importe qui pouvant écrire
// le spec écrirait aussi le status, et la séparation qui fait tout l'intérêt de
// l'opérateur n'existerait pas.
func TestStatusIsASubresource(t *testing.T) {
	ctx := context.Background()
	vc := newVCluster("status-subresource", nil)
	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, vc) }()

	// Écrire le status via un Update ordinaire ne doit rien changer.
	vc.Status.Phase = v1alpha1.VClusterPhaseReady
	if err := k8sClient.Update(ctx, vc); err != nil {
		t.Fatalf("update: %v", err)
	}
	var relu v1alpha1.VCluster
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vc), &relu); err != nil {
		t.Fatalf("get: %v", err)
	}
	if relu.Status.Phase != "" {
		t.Fatalf("le status a été écrit par un Update ordinaire (phase=%q) : ce n'est pas une sous-ressource", relu.Status.Phase)
	}

	// Par la sous-ressource, en revanche, il doit passer.
	relu.Status.Phase = v1alpha1.VClusterPhaseReady
	if err := k8sClient.Status().Update(ctx, &relu); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vc), &relu); err != nil {
		t.Fatalf("get: %v", err)
	}
	if relu.Status.Phase != v1alpha1.VClusterPhaseReady {
		t.Fatalf("phase = %q après écriture via /status", relu.Status.Phase)
	}
}
