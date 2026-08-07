package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/vault"
)

// newIntegrationsTestService builds a Service with no external clients wired
// at all — enough to exercise the "not configured" guards without a real
// Vault, Keycloak or cluster.
func newIntegrationsTestService(cfg *config.Config) *Service {
	var mu sync.RWMutex
	if cfg == nil {
		cfg = &config.Config{}
	}
	return New(Deps{Cfg: cfg, K8sClients: map[string]*kubernetes.StatusClient{}, K8sClientsMu: &mu})
}

func TestVaultAuthConfigured_NoClient(t *testing.T) {
	s := newIntegrationsTestService(nil)
	_, err := s.VaultAuthConfigured(context.Background(), "demo", "preprod")
	if !errors.Is(err, ErrVaultNotConfigured) {
		t.Fatalf("erreur = %v, attendu ErrVaultNotConfigured", err)
	}
}

func TestVaultWebhookReady_NoK8sClient(t *testing.T) {
	s := newIntegrationsTestService(nil)
	_, err := s.VaultWebhookReady(context.Background(), "demo", "preprod")
	if !errors.Is(err, ErrK8sUnavailable) {
		t.Fatalf("erreur = %v, attendu ErrK8sUnavailable", err)
	}
}

func TestConfigureVaultAuth_NoClient(t *testing.T) {
	s := newIntegrationsTestService(nil)
	if err := s.ConfigureVaultAuth(context.Background(), "demo", "preprod"); !errors.Is(err, ErrVaultNotConfigured) {
		t.Fatalf("erreur = %v, attendu ErrVaultNotConfigured", err)
	}
}

func TestEnsureKeycloakClient_NoClient(t *testing.T) {
	s := newIntegrationsTestService(nil)
	if err := s.EnsureKeycloakClient("demo", "preprod"); !errors.Is(err, ErrKeycloakNotConfigured) {
		t.Fatalf("erreur = %v, attendu ErrKeycloakNotConfigured", err)
	}
}

// vclusterAPIHost ne doit jamais mélanger les deux domaines : la promotion
// preprod→prod dépend de cette dérivation qui reste correcte à chaque appel,
// jamais gelée dans un champ (crd-vcluster.md §2.2).
func TestVclusterAPIHost_PicksTheRightDomain(t *testing.T) {
	s := newIntegrationsTestService(&config.Config{
		BaseDomainPreprod: "preprod.example.com",
		BaseDomainProd:    "example.com",
	})

	if got, want := s.vclusterAPIHost("demo", "preprod"), "https://demo.api.preprod.example.com"; got != want {
		t.Fatalf("apiHost preprod = %q, attendu %q", got, want)
	}
	if got, want := s.vclusterAPIHost("demo", "prod"), "https://demo.api.example.com"; got != want {
		t.Fatalf("apiHost prod = %q, attendu %q", got, want)
	}
}

// VaultAuthConfigured délègue bien à Vault, sans rien inventer localement : un
// backend absent (HTTP 404 sur sys/auth ne le liste pas) répond false, sans
// erreur.
func TestVaultAuthConfigured_AsksVault(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kubernetes-vcluster-autre-preprod/": map[string]any{"type": "kubernetes"},
		})
	}))
	defer srv.Close()

	s := newIntegrationsTestService(nil)
	s.vault = vault.NewClient(srv.URL, "test-token")

	exists, err := s.VaultAuthConfigured(context.Background(), "demo", "preprod")
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if exists {
		t.Fatal("backend rapporté existant alors que seul un autre chemin est listé")
	}
	if gotPath != "/v1/sys/auth" {
		t.Fatalf("chemin interrogé = %q, attendu /v1/sys/auth", gotPath)
	}

	exists, err = s.VaultAuthConfigured(context.Background(), "autre", "preprod")
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if !exists {
		t.Fatal("backend non trouvé alors qu'il est listé par Vault")
	}
}

// Ce que les intégrations rapportent avec le câblage RÉEL de l'opérateur.
//
// Même discipline que vcluster_deletion_wiring_test.go, et pour la même raison :
// cmd/operator/main.go construit le service sans clients Vault, Keycloak ni
// Rancher. Ce test dit ce que ça donne, pour que ce soit un fait écrit et non une
// surprise de production.
//
// Ce qu'on vérifie n'est PAS que ça marche — ça ne peut pas — mais que ça ne
// prétend pas marcher. Un « configuré » optimiste ici produirait exactement le
// faux vert que N4 dénonçait, une couche plus bas.
func TestOperatorWiringCannotRunTheIntegrationsAndSaysSo(t *testing.T) {
	ctx := context.Background()
	var mu sync.RWMutex
	s := New(Deps{
		Cfg:          &config.Config{RancherEnabledPreprod: true},
		K8sClients:   map[string]*kubernetes.StatusClient{"preprod": kubernetes.NewTestStatusClient()},
		K8sClientsMu: &mu,
	})

	if err := s.EnsureKeycloakClient("demo", "preprod"); !errors.Is(err, ErrKeycloakNotConfigured) {
		t.Errorf("EnsureKeycloakClient = %v, attendu ErrKeycloakNotConfigured : sans erreur "+
			"nommée, l'opérateur écrirait ArgoCDReady=True sans client OIDC", err)
	}

	if _, err := s.VaultAuthConfigured(ctx, "demo", "preprod"); err == nil {
		t.Error("VaultAuthConfigured ne signale pas l'absence de client Vault : " +
			"« pas de backend d'auth » et « je ne peux pas regarder » deviendraient identiques")
	}

	// Rancher : la cell l'annonce actif, le processus n'a pas de client. Le champ
	// NotConfigured est ce qui distingue ça d'un « rien à appairer ».
	if st := s.GetRancherStatus(ctx, "demo", "preprod"); !st.NotConfigured {
		t.Error("GetRancherStatus ne signale pas le client manquant")
	}
}
