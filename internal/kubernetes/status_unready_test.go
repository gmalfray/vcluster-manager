package kubernetes

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// newFluxObj fabrique un objet Flux (HelmRelease ou Kustomization) porteur
// d'une condition Ready.
func newFluxObj(apiVersion, kind, name, namespace, readyStatus, message string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  readyStatus,
					"message": message,
				},
			},
		},
	}}
}

func newHelmRelease(name, namespace, readyStatus, message string) *unstructured.Unstructured {
	return newFluxObj("helm.toolkit.fluxcd.io/v2", "HelmRelease", name, namespace, readyStatus, message)
}

func newKustomization(name, namespace, readyStatus, message string) *unstructured.Unstructured {
	return newFluxObj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", name, namespace, readyStatus, message)
}

// TestListUnreadyReconciliations_NamesWhatIsBroken rejoue la situation réelle
// du 2026-08-08 : sur deux vclusters, les HelmReleases cert-manager étaient en
// échec depuis des heures et la Kustomization des ClusterIssuer tombait à leur
// suite. Le tableau de bord affichait le bon compteur, en ambre, et personne
// n'a pu en tirer quoi que ce soit — un chiffre dit qu'il faut chercher, pas où.
//
// Ce que le test verrouille : les objets en échec sont nommés, ceux qui vont
// bien ne polluent pas la liste, et le message qui dit quoi faire est conservé.
func TestListUnreadyReconciliations_NamesWhatIsBroken(t *testing.T) {
	s := NewTestStatusClient(
		newHelmRelease("vcluster-demo", "vcluster-demo", "True", ""),
		newHelmRelease("cert-manager", "vcluster-recette-restore-a", "False",
			`HelmChart 'cert-manager/...' is not ready: failed to get source: HelmRepository "jetstack" not found`),
		newKustomization("cert-manager-config-recette-restore-a", "vcluster-recette-restore-a", "False",
			`ClusterIssuer/letsencrypt dry-run failed: no matches for kind "ClusterIssuer"`),
		// Hors des namespaces vcluster-* : la plateforme elle-même n'est pas
		// le sujet de cet écran, qui parle des vclusters.
		newHelmRelease("rancher", "cattle-system", "False", "peu importe"),
	)

	got, err := s.ListUnreadyReconciliations(context.Background())
	if err != nil {
		t.Fatalf("ListUnreadyReconciliations: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 unready reconciliations, got %d: %+v", len(got), got)
	}

	names := map[string]UnreadyReconciliation{}
	for _, u := range got {
		names[u.Kind+"/"+u.Name] = u
	}

	hr, ok := names["HelmRelease/cert-manager"]
	if !ok {
		t.Fatal("le HelmRelease cert-manager en échec n'est pas nommé — c'est exactement ce qu'on cherchait à voir")
	}
	if !strings.Contains(hr.Message, "jetstack") {
		t.Errorf("le message qui dit quoi faire est perdu : %q", hr.Message)
	}
	if hr.Namespace != "vcluster-recette-restore-a" {
		t.Errorf("namespace attendu vcluster-recette-restore-a, got %q", hr.Namespace)
	}

	if _, ok := names["Kustomization/cert-manager-config-recette-restore-a"]; !ok {
		t.Error("la Kustomization en échec est absente : ne lister que les HelmReleases " +
			"raterait la moitié des symptômes que cette panne offrait")
	}

	for _, u := range got {
		if u.Name == "vcluster-demo" {
			t.Error("une réconciliation saine s'est retrouvée dans la liste des échecs")
		}
		if u.Namespace == "cattle-system" {
			t.Error("un objet hors des namespaces vcluster-* a été remonté")
		}
	}
}

// TestListUnreadyReconciliations_StableOrder : la page se rafraîchit toute
// seule. Sans tri, l'itération d'une map et celle de l'API feraient danser la
// liste à chaque rafraîchissement, ce qui la rend illisible au moment précis
// où on essaie de la lire.
func TestListUnreadyReconciliations_StableOrder(t *testing.T) {
	s := NewTestStatusClient(
		newHelmRelease("zeta", "vcluster-b", "False", ""),
		newHelmRelease("alpha", "vcluster-a", "False", ""),
		newKustomization("beta", "vcluster-a", "False", ""),
	)

	var first []string
	for i := 0; i < 5; i++ {
		got, err := s.ListUnreadyReconciliations(context.Background())
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		var order []string
		for _, u := range got {
			order = append(order, u.Namespace+"/"+u.Kind+"/"+u.Name)
		}
		if i == 0 {
			first = order
			continue
		}
		if strings.Join(order, ",") != strings.Join(first, ",") {
			t.Fatalf("ordre instable entre deux appels :\n  %v\n  %v", first, order)
		}
	}
}
