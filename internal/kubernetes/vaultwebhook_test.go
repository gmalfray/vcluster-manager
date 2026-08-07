package kubernetes

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// vaultWebhookKustomization and vaultWebhookHelmRelease build the two objects
// VaultWebhookReady reads, with the Ready condition set to readyStatus
// ("True" or anything else — extractConditionStatus only special-cases
// "True").
func vaultWebhookKustomization(name, readyStatus string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata": map[string]interface{}{
			"name":      "vault-webhook-" + name,
			"namespace": "vcluster-" + name,
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": readyStatus},
			},
		},
	}}
}

func vaultWebhookHelmRelease(name, readyStatus string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "helm.toolkit.fluxcd.io/v2",
		"kind":       "HelmRelease",
		"metadata": map[string]interface{}{
			"name":      "vault-webhook",
			"namespace": "vcluster-" + name,
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": readyStatus},
			},
		},
	}}
}

// Les deux objets sont Ready : c'est le seul cas où VaultWebhookReady répond
// vrai — c'est le prérequis avant de générer un token reviewer.
func TestVaultWebhookReady_TrueWhenBothReady(t *testing.T) {
	s := NewTestStatusClient(
		vaultWebhookKustomization("demo", "True"),
		vaultWebhookHelmRelease("demo", "True"),
	)
	ready, err := s.VaultWebhookReady(context.Background(), "demo")
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if !ready {
		t.Fatal("attendu prêt : les deux objets sont Ready")
	}
}

// La Kustomization seule ne suffit pas : le HelmRelease qu'elle applique à
// l'intérieur du vcluster doit lui aussi être Ready.
func TestVaultWebhookReady_FalseWhenHelmReleaseNotReady(t *testing.T) {
	s := NewTestStatusClient(
		vaultWebhookKustomization("demo", "True"),
		vaultWebhookHelmRelease("demo", "False"),
	)
	ready, err := s.VaultWebhookReady(context.Background(), "demo")
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if ready {
		t.Fatal("attendu pas prêt : le HelmRelease n'est pas Ready")
	}
}

// Rien n'a encore été créé : pas prêt, mais pas une erreur — c'est le cas
// normal juste après la création du vcluster, pas une panne.
func TestVaultWebhookReady_FalseWhenNothingExistsYet(t *testing.T) {
	s := NewTestStatusClient()
	ready, err := s.VaultWebhookReady(context.Background(), "tout-neuf")
	if err != nil {
		t.Fatalf("l'absence de la Kustomization ne devrait pas être une erreur : %v", err)
	}
	if ready {
		t.Fatal("attendu pas prêt : rien n'existe encore")
	}
}

// La Kustomization existe et est Ready, mais pas encore le HelmRelease : pas
// prêt, toujours sans erreur — c'est l'état intermédiaire attendu pendant le
// déploiement.
func TestVaultWebhookReady_FalseWhenHelmReleaseMissing(t *testing.T) {
	s := NewTestStatusClient(vaultWebhookKustomization("demo", "True"))
	ready, err := s.VaultWebhookReady(context.Background(), "demo")
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if ready {
		t.Fatal("attendu pas prêt : le HelmRelease n'existe pas encore")
	}
}
