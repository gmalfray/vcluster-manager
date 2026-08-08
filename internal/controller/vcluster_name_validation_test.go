package controller

import (
	"context"
	"strings"
	"testing"
)

// La règle CEL sur `metadata.name` (audit N6, TODO.md « Opérateur — durcissement
// issu de l'audit N6 ») doit refuser à l'admission ce que le contrôleur refusait
// jusqu'ici après coup : un CR accepté puis bloqué au reconcile laisse un
// `Accepted=False` qui traîne au lieu d'un refus net au `kubectl apply`.
//
// `newVCluster` vient de vcluster_schema_test.go, même paquet.

func TestSchemaAcceptsAValidName(t *testing.T) {
	ctx := context.Background()
	vc := newVCluster("mon-vcluster-42", nil)

	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("un nom valide a été refusé : %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, vc) }()
}

// Un nom de 54 caractères est le maximum accepté : "vcluster-" (9) + 54 = 63,
// la limite RFC 1123 d'une étiquette DNS, donc du namespace que le nom sert à
// dériver.
func TestSchemaAcceptsNameAtTheFiftyFourCharBoundary(t *testing.T) {
	ctx := context.Background()
	nom := "a" + strings.Repeat("a", 53) // 54 caractères au total
	if len(nom) != 54 {
		t.Fatalf("le nom de test fait %d caractères, pas 54 — corriger le test avant de lire le résultat", len(nom))
	}
	vc := newVCluster(nom, nil)

	if err := k8sClient.Create(ctx, vc); err != nil {
		t.Fatalf("un nom de 54 caractères a été refusé alors qu'il tient exactement dans vcluster-<nom> ≤ 63 : %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, vc) }()
}

// Un caractère de plus fait dépasser les 63 de "vcluster-" + nom : la création
// du namespace échouerait au reconcile si l'admission ne l'arrêtait pas avant.
func TestSchemaRefusesNameOverTheFiftyFourCharBoundary(t *testing.T) {
	ctx := context.Background()
	nom := "a" + strings.Repeat("a", 54) // 55 caractères au total
	if len(nom) != 55 {
		t.Fatalf("le nom de test fait %d caractères, pas 55 — corriger le test avant de lire le résultat", len(nom))
	}
	vc := newVCluster(nom, nil)

	err := k8sClient.Create(ctx, vc)
	if err == nil {
		_ = k8sClient.Delete(ctx, vc)
		t.Fatal("un nom de 55 caractères a été accepté — vcluster-<nom> dépasserait les 63 caractères d'un namespace")
	}
	if !strings.Contains(err.Error(), "54 caractères") {
		t.Fatalf("refusé, mais le message n'explique pas la limite : %v", err)
	}
}

func TestSchemaRefusesUppercaseName(t *testing.T) {
	ctx := context.Background()
	vc := newVCluster("Majuscule", nil)

	err := k8sClient.Create(ctx, vc)
	if err == nil {
		_ = k8sClient.Delete(ctx, vc)
		t.Fatal("un nom en majuscules a été accepté — vcluster-Majuscule n'est pas un namespace valide")
	}
	if !strings.Contains(err.Error(), "nom invalide") {
		t.Fatalf("refusé, mais le message n'explique pas pourquoi : %v", err)
	}
}

func TestSchemaRefusesNameStartingWithADigit(t *testing.T) {
	ctx := context.Background()
	vc := newVCluster("1demo", nil)

	if err := k8sClient.Create(ctx, vc); err == nil {
		_ = k8sClient.Delete(ctx, vc)
		t.Fatal("un nom commençant par un chiffre a été accepté")
	}
}

// Non-régression : la règle de nom ajoutée ici ne doit pas remplacer la règle
// dédiée « manager » (vcluster_types.go), qui reste une règle CEL distincte.
// « manager » respecte la forme `^[a-z][a-z0-9-]{0,53}$` — c'est l'autre règle
// qui doit l'arrêter.
func TestSchemaStillRefusesReservedNameManager(t *testing.T) {
	ctx := context.Background()
	vc := newVCluster("manager", nil)

	err := k8sClient.Create(ctx, vc)
	if err == nil {
		_ = k8sClient.Delete(ctx, vc)
		t.Fatal("« manager » a été accepté — c'est le namespace de l'opérateur lui-même")
	}
	if !strings.Contains(err.Error(), "réservé") {
		t.Fatalf("refusé, mais le message n'explique pas pourquoi : %v", err)
	}
}

// Le message doit se lire directement dans la sortie de `kubectl apply`, sans
// aller chercher la CRD pour comprendre ce qui a été refusé.
func TestSchemaNameErrorMessageIsReadable(t *testing.T) {
	ctx := context.Background()
	vc := newVCluster("Pas_Valide!", nil)

	err := k8sClient.Create(ctx, vc)
	if err == nil {
		_ = k8sClient.Delete(ctx, vc)
		t.Fatal("un nom invalide a été accepté")
	}
	for _, mot := range []string{"nom invalide", "minuscule", "54 caractères"} {
		if !strings.Contains(err.Error(), mot) {
			t.Fatalf("message d'erreur incomplet, %q absent : %v", mot, err)
		}
	}
}
