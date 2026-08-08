package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/vault"
)

// errAppRoleUnreachable simule l'échec réseau du login AppRole, sans faire un
// vrai appel HTTP dans le test.
var errAppRoleUnreachable = errors.New("dial tcp: connection refused")

// baseCfg construit une configuration sans aucune intégration réglée — le cas
// d'un opérateur qui tourne sans Vault, Keycloak ni Rancher.
func baseCfg() *config.Config {
	return &config.Config{}
}

func TestWireKeycloak_AbsenceEstLegitime(t *testing.T) {
	kc, err := wireKeycloak(baseCfg())
	if err != nil {
		t.Fatalf("erreur = %v, voulu nil : l'absence de configuration n'est pas une panne", err)
	}
	if kc != nil {
		t.Fatalf("client = %v, voulu nil sans identifiants", kc)
	}
}

// URL et Realm sont renseignés dans ces deux tests pour que seule la
// condition ClientID/ClientSecret décide de l'issue — sinon un test qui
// laisse aussi l'URL vide passerait pour la mauvaise raison (la garde
// suivante, sur URL/Realm, prendrait le relais et masquerait un `||` muté en
// `&&` sur celle-ci).
func TestWireKeycloak_ClientIDSeulEchoue(t *testing.T) {
	cfg := baseCfg()
	cfg.KeycloakClientID = "vcluster-manager-service"
	cfg.KeycloakURL = "https://keycloak.rebuild-it.fr"
	cfg.KeycloakRealm = "vcluster-manager"
	if _, err := wireKeycloak(cfg); err == nil {
		t.Fatal("erreur = nil, voulu une erreur : le secret manque")
	}
}

func TestWireKeycloak_ClientSecretSeulEchoue(t *testing.T) {
	cfg := baseCfg()
	cfg.KeycloakClientSecret = "s3cr3t"
	cfg.KeycloakURL = "https://keycloak.rebuild-it.fr"
	cfg.KeycloakRealm = "vcluster-manager"
	if _, err := wireKeycloak(cfg); err == nil {
		t.Fatal("erreur = nil, voulu une erreur : l'ID client manque")
	}
}

func TestWireKeycloak_URLManquanteEchoue(t *testing.T) {
	cfg := baseCfg()
	cfg.KeycloakClientID = "id"
	cfg.KeycloakClientSecret = "secret"
	cfg.KeycloakRealm = "vcluster-manager"
	// KeycloakURL laissé vide.
	if _, err := wireKeycloak(cfg); err == nil {
		t.Fatal("erreur = nil, voulu une erreur : KEYCLOAK_URL manque alors que les identifiants sont présents")
	}
}

func TestWireKeycloak_RealmManquantEchoue(t *testing.T) {
	cfg := baseCfg()
	cfg.KeycloakClientID = "id"
	cfg.KeycloakClientSecret = "secret"
	cfg.KeycloakURL = "https://keycloak.rebuild-it.fr"
	// KeycloakRealm laissé vide.
	if _, err := wireKeycloak(cfg); err == nil {
		t.Fatal("erreur = nil, voulu une erreur : KEYCLOAK_REALM manque alors que les identifiants sont présents")
	}
}

func TestWireKeycloak_ConfigCompleteConstruitLeClient(t *testing.T) {
	cfg := baseCfg()
	cfg.KeycloakClientID = "vcluster-manager-service"
	cfg.KeycloakClientSecret = "secret"
	cfg.KeycloakURL = "https://keycloak.rebuild-it.fr"
	cfg.KeycloakRealm = "vcluster-manager"
	kc, err := wireKeycloak(cfg)
	if err != nil {
		t.Fatalf("erreur = %v, voulu nil : la configuration est complète", err)
	}
	if kc == nil {
		t.Fatal("client = nil, voulu un client construit")
	}
}

func TestWireRancher_AbsenceEstLegitime(t *testing.T) {
	rc, err := wireRancher(baseCfg())
	if err != nil {
		t.Fatalf("erreur = %v, voulu nil : l'absence de configuration n'est pas une panne", err)
	}
	if rc != nil {
		t.Fatalf("client = %v, voulu nil sans URL/token", rc)
	}
}

func TestWireRancher_URLSeuleEchoue(t *testing.T) {
	cfg := baseCfg()
	cfg.RancherURL = "https://rancher.rebuild-it.fr"
	if _, err := wireRancher(cfg); err == nil {
		t.Fatal("erreur = nil, voulu une erreur : le token manque")
	}
}

func TestWireRancher_TokenSeulEchoue(t *testing.T) {
	cfg := baseCfg()
	cfg.RancherToken = "token"
	if _, err := wireRancher(cfg); err == nil {
		t.Fatal("erreur = nil, voulu une erreur : l'URL manque")
	}
}

func TestWireRancher_ConfigCompleteConstruitLeClient(t *testing.T) {
	cfg := baseCfg()
	cfg.RancherURL = "https://rancher.rebuild-it.fr"
	cfg.RancherToken = "token"
	rc, err := wireRancher(cfg)
	if err != nil {
		t.Fatalf("erreur = %v, voulu nil : la configuration est complète", err)
	}
	if rc == nil {
		t.Fatal("client = nil, voulu un client construit")
	}
}

func TestWireVault_AbsenceEstLegitime(t *testing.T) {
	vc, err := wireVault(context.Background(), baseCfg())
	if err != nil {
		t.Fatalf("erreur = %v, voulu nil : l'absence de configuration n'est pas une panne", err)
	}
	if vc != nil {
		t.Fatalf("client = %v, voulu nil sans VAULT_ADDR", vc)
	}
}

func TestWireVault_AddrSansCredsEchoue(t *testing.T) {
	cfg := baseCfg()
	cfg.VaultAddr = "https://vault.rebuild-it.fr"
	if _, err := wireVault(context.Background(), cfg); err == nil {
		t.Fatal("erreur = nil, voulu une erreur : VAULT_ADDR est défini sans aucun moyen de s'authentifier")
	}
}

// Le constructeur AppRole est substitué et fait échouer le test s'il est
// appelé : un role_id seul, sans secret_id, ne doit jamais déclencher de
// login. Sans ce garde, le test dépendrait d'un vrai appel réseau vers Vault
// pour obtenir son erreur — non déterministe, et ça a déjà laissé passer un
// mutant (RoleID != "" && SecretID != "" muté en ||) qui échouait "par
// chance" faute de réseau, pas parce que le test le détectait.
func TestWireVault_AppRolePartielSansTokenEchoue(t *testing.T) {
	cfg := baseCfg()
	cfg.VaultAddr = "https://vault.rebuild-it.fr"
	cfg.VaultRoleID = "role-id" // secret_id manquant, pas de token
	restore := newVaultClientWithAppRole
	newVaultClientWithAppRole = func(context.Context, string, string, string) (*vault.Client, error) {
		t.Fatal("newVaultClientWithAppRole appelé avec un AppRole incomplet (secret_id manquant)")
		return nil, nil
	}
	defer func() { newVaultClientWithAppRole = restore }()

	if _, err := wireVault(context.Background(), cfg); err == nil {
		t.Fatal("erreur = nil, voulu une erreur : l'AppRole est incomplet et il n'y a pas de token de repli")
	}
}

func TestWireVault_TokenStatiqueConstruitLeClientSansAppelReseau(t *testing.T) {
	cfg := baseCfg()
	cfg.VaultAddr = "https://vault.rebuild-it.fr"
	cfg.VaultToken = "s.abc123"
	// Le constructeur AppRole ne doit pas être appelé sur ce chemin : le
	// substitut panique s'il l'est, ce qui ferait tomber le test au lieu de
	// le laisser passer silencieusement sur un mauvais chemin.
	restore := newVaultClientWithAppRole
	newVaultClientWithAppRole = func(context.Context, string, string, string) (*vault.Client, error) {
		t.Fatal("newVaultClientWithAppRole appelé alors que seul VAULT_TOKEN est défini")
		return nil, nil
	}
	defer func() { newVaultClientWithAppRole = restore }()

	vc, err := wireVault(context.Background(), cfg)
	if err != nil {
		t.Fatalf("erreur = %v, voulu nil : un token statique suffit à construire le client", err)
	}
	if vc == nil {
		t.Fatal("client = nil, voulu un client construit avec le token statique")
	}
}

func TestWireVault_AppRoleAppelleLeConstructeurAvecLesBonsArguments(t *testing.T) {
	cfg := baseCfg()
	cfg.VaultAddr = "https://vault.rebuild-it.fr"
	cfg.VaultRoleID = "role-id"
	cfg.VaultSecretID = "secret-id"

	var gotAddr, gotRole, gotSecret string
	restore := newVaultClientWithAppRole
	newVaultClientWithAppRole = func(_ context.Context, addr, roleID, secretID string) (*vault.Client, error) {
		gotAddr, gotRole, gotSecret = addr, roleID, secretID
		return vault.NewClient(addr, "fake-token-from-approle"), nil
	}
	defer func() { newVaultClientWithAppRole = restore }()

	vc, err := wireVault(context.Background(), cfg)
	if err != nil {
		t.Fatalf("erreur = %v, voulu nil", err)
	}
	if vc == nil {
		t.Fatal("client = nil, voulu le client renvoyé par le constructeur AppRole")
	}
	if gotAddr != cfg.VaultAddr || gotRole != cfg.VaultRoleID || gotSecret != cfg.VaultSecretID {
		t.Fatalf("arguments transmis = (%q, %q, %q), voulu (%q, %q, %q)",
			gotAddr, gotRole, gotSecret, cfg.VaultAddr, cfg.VaultRoleID, cfg.VaultSecretID)
	}
}

func TestWireVault_EchecAppRolePropageLErreurEtNeConstruitAucunClient(t *testing.T) {
	cfg := baseCfg()
	cfg.VaultAddr = "https://vault.rebuild-it.fr"
	cfg.VaultRoleID = "role-id"
	cfg.VaultSecretID = "secret-id"

	restore := newVaultClientWithAppRole
	newVaultClientWithAppRole = func(context.Context, string, string, string) (*vault.Client, error) {
		return nil, errAppRoleUnreachable
	}
	defer func() { newVaultClientWithAppRole = restore }()

	vc, err := wireVault(context.Background(), cfg)
	if err == nil {
		t.Fatal("erreur = nil, voulu la propagation de l'échec du login AppRole")
	}
	if vc != nil {
		t.Fatalf("client = %v, voulu nil quand le login a échoué", vc)
	}
	if !strings.Contains(err.Error(), errAppRoleUnreachable.Error()) {
		t.Fatalf("erreur = %q, voulu qu'elle porte la cause d'origine %q", err, errAppRoleUnreachable)
	}
}

// TestWireIntegrations_UneSeuleIntegrationInvalideFaitEchouerLeTout vérifie
// qu'une configuration Vault incohérente fait échouer wireIntegrations même
// quand Keycloak et Rancher sont, eux, correctement réglés (ou absents) — le
// démarrage ne doit pas réussir à moitié.
func TestWireIntegrations_UneSeuleIntegrationInvalideFaitEchouerLeTout(t *testing.T) {
	cfg := baseCfg()
	cfg.VaultAddr = "https://vault.rebuild-it.fr" // pas de creds : invalide
	cfg.RancherURL = "https://rancher.rebuild-it.fr"
	cfg.RancherToken = "token" // valide

	out, err := wireIntegrations(context.Background(), cfg)
	if err == nil {
		t.Fatal("erreur = nil, voulu un échec : Vault est mal configuré")
	}
	if out.vault != nil {
		t.Fatalf("client Vault = %v, voulu nil quand la construction a échoué", out.vault)
	}
}

// TestWireIntegrations_LesTroisErreursSontJointes vérifie que
// wireIntegrations n'abandonne pas au premier échec : les trois messages
// doivent être lisibles dans l'erreur combinée, pour ne pas cacher un
// deuxième problème derrière le premier corrigé.
func TestWireIntegrations_LesTroisErreursSontJointes(t *testing.T) {
	cfg := baseCfg()
	cfg.KeycloakClientID = "id"                      // secret manquant : invalide
	cfg.RancherURL = "https://rancher.rebuild-it.fr" // token manquant : invalide
	cfg.VaultAddr = "https://vault.rebuild-it.fr"    // creds manquants : invalide

	_, err := wireIntegrations(context.Background(), cfg)
	if err == nil {
		t.Fatal("erreur = nil, voulu un échec portant les trois causes")
	}
	for _, want := range []string{"keycloak", "rancher", "vault"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("erreur = %q, ne mentionne pas %q — une des trois défaillances a été perdue en route", err, want)
		}
	}
}

func TestWireIntegrations_ToutAbsentNeConstruitRienEtNEchouePas(t *testing.T) {
	out, err := wireIntegrations(context.Background(), baseCfg())
	if err != nil {
		t.Fatalf("erreur = %v, voulu nil : rien n'est configuré, ce n'est pas une panne", err)
	}
	if out.keycloak != nil || out.rancher != nil || out.vault != nil {
		t.Fatalf("clients = %+v, voulu les trois à nil", out)
	}
}
