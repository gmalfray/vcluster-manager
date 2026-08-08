package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/keycloak"
	"github.com/gmalfray/vcluster-manager/internal/rancher"
	"github.com/gmalfray/vcluster-manager/internal/vault"
)

// newVaultClientWithAppRole rend vault.NewClientWithAppRole substituable en
// test. La vraie fonction fait un appel HTTP de login au démarrage — ce
// qu'un test unitaire n'a pas à reproduire pour vérifier le CÂBLAGE (quelle
// branche de configuration est prise, quelle erreur remonte), seul objet de
// ce fichier.
var newVaultClientWithAppRole = vault.NewClientWithAppRole

// integrationClients regroupe les trois clients que reconcileIntegrations
// (internal/controller/vcluster_integrations.go) consomme au travers de
// service.Deps. Un champ nil veut dire « pas configuré » — l'étape
// correspondante le sait déjà et rend Unknown/NotConfigured, ce qui est le
// comportement voulu, pas une dégradation à corriger.
//
// GitLab n'y figure pas : aucune étape du reconcile n'appelle de client
// GitLab (vérifié dans vcluster_integrations.go, pas recopié depuis
// cmd/server), et le câbler transformerait un hoquet d'API GitLab en
// ArgoCDReady=False lisible comme « ArgoCD est cassé » — voir
// AppManifestsRepoExists dans internal/gitops/gitlab.go et
// docs/etat-brique-operateur.md, §« Ce qui n'est pas couvert ».
type integrationClients struct {
	keycloak *keycloak.Client
	rancher  *rancher.Client
	vault    *vault.Client
}

// wireIntegrations construit les clients Vault, Keycloak et Rancher à partir
// de la configuration lue par LoadOperator, ou refuse de démarrer si elle est
// incohérente.
//
// La règle est la même pour les trois : l'absence totale de configuration
// est un état légitime (client nil, pas d'erreur) — un opérateur sans Vault
// tourne très bien, et l'étape de reconcile concernée le rend honnêtement en
// Unknown/NotConfigured. Une configuration PARTIELLE (une URL sans les
// identifiants qui vont avec, ou l'inverse) est en revanche une erreur de
// démarrage, pas un simple avertissement : un opérateur qui démarre quand même
// produirait des Unknown identiques à ceux d'une absence de configuration, et
// plus personne ne pourrait distinguer « pas de Vault ici » de « le secret
// Vault est mal monté ». C'est le point sur lequel ce câblage diverge de
// cmd/server, qui journalise et continue sans client dans ce cas — voir le
// rapport de la mission pour le détail des deux comportements.
func wireIntegrations(ctx context.Context, cfg *config.Config) (integrationClients, error) {
	var out integrationClients
	var errs []error

	if kc, err := wireKeycloak(cfg); err != nil {
		errs = append(errs, err)
	} else {
		out.keycloak = kc
	}

	if rc, err := wireRancher(cfg); err != nil {
		errs = append(errs, err)
	} else {
		out.rancher = rc
	}

	if vc, err := wireVault(ctx, cfg); err != nil {
		errs = append(errs, err)
	} else {
		out.vault = vc
	}

	return out, errors.Join(errs...)
}

// wireKeycloak construit le client OIDC ArgoCD (EnsureKeycloakClient,
// vcluster_integrations.go) — même identifiants service-account que
// cmd/server, jamais le client OIDC applicatif (OIDC_CLIENT_ID/SECRET), qui
// reste hors de portée : l'opérateur n'authentifie personne.
func wireKeycloak(cfg *config.Config) (*keycloak.Client, error) {
	switch {
	case cfg.KeycloakClientID == "" && cfg.KeycloakClientSecret == "":
		return nil, nil
	case cfg.KeycloakClientID == "" || cfg.KeycloakClientSecret == "":
		return nil, errors.New("keycloak : KEYCLOAK_CLIENT_ID et KEYCLOAK_CLIENT_SECRET doivent être définis ensemble, ou pas du tout")
	case cfg.KeycloakURL == "" || cfg.KeycloakRealm == "":
		return nil, errors.New("keycloak : identifiants présents mais KEYCLOAK_URL ou KEYCLOAK_REALM manquant")
	}
	return keycloak.NewClient(cfg.KeycloakURL, cfg.KeycloakRealm, cfg.KeycloakClientID, cfg.KeycloakClientSecret,
		cfg.BaseDomainPreprod, cfg.BaseDomainProd), nil
}

// wireRancher construit le client d'appairage (GetRancherStatus/PairRancher,
// vcluster_integrations.go). RancherEnabledForEnv (déjà lu depuis la même
// configuration) reste le contrôle par cell, appliqué au moment de l'appel,
// pas ici — ne pas construire le client parce qu'une cell l'a désactivé
// empêcherait de voir la vraie raison dans la condition écrite.
func wireRancher(cfg *config.Config) (*rancher.Client, error) {
	switch {
	case cfg.RancherURL == "" && cfg.RancherToken == "":
		return nil, nil
	case cfg.RancherURL == "" || cfg.RancherToken == "":
		return nil, errors.New("rancher : RANCHER_URL et RANCHER_TOKEN doivent être définis ensemble, ou pas du tout")
	}
	return rancher.NewClient(cfg.RancherURL, cfg.RancherToken), nil
}

// wireVault construit le client Vault (VaultAuthConfigured/ConfigureVaultAuth,
// vcluster_integrations.go). AppRole est préféré, comme cmd/server ; le login
// AppRole est un appel réseau réel, donc une erreur ici est déjà celle que
// subirait le premier reconcile — mieux vaut la voir au démarrage, dans les
// logs du pod, qu'au bout d'une boucle de requeue.
func wireVault(ctx context.Context, cfg *config.Config) (*vault.Client, error) {
	if cfg.VaultAddr == "" {
		return nil, nil
	}
	switch {
	case cfg.VaultRoleID != "" && cfg.VaultSecretID != "":
		vc, err := newVaultClientWithAppRole(ctx, cfg.VaultAddr, cfg.VaultRoleID, cfg.VaultSecretID)
		if err != nil {
			return nil, fmt.Errorf("vault : authentification AppRole : %w", err)
		}
		return vc, nil
	case cfg.VaultToken != "":
		return vault.NewClient(cfg.VaultAddr, cfg.VaultToken), nil
	default:
		return nil, errors.New("vault : VAULT_ADDR défini sans VAULT_ROLE_ID et VAULT_SECRET_ID à la fois, ni VAULT_TOKEN")
	}
}

// logIntegrationOutcome journalise, pour une intégration, si un client a été
// câblé ou non — sans jamais laisser entendre qu'une absence est une panne.
// hint est la valeur de configuration qui a fait pencher la décision (une
// URL, jamais un secret) : utile pour vérifier en recette que c'est bien le
// bon environnement qui a été lu, sans exposer d'identifiant dans les logs.
func logIntegrationOutcome(log logr.Logger, name string, configured bool, hint string) {
	if configured {
		log.Info("intégration câblée", "integration", name, "hint", hint)
		return
	}
	log.Info("intégration non configurée : l'étape correspondante rendra Unknown/NotConfigured", "integration", name)
}
