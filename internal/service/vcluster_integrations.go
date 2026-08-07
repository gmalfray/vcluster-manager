package service

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for the two integrations that, like Rancher, are optional at
// the platform level: the operator instance may simply not have a client
// wired (config gap, not a runtime failure). Kept apart from ErrK8sUnavailable
// because the caller treats them differently — a config gap is reported as
// Unknown and does not retry, an unreachable Vault/Keycloak does.
var (
	// ErrVaultNotConfigured means no Vault client is configured on this operator.
	ErrVaultNotConfigured = errors.New("vault client not configured")
	// ErrKeycloakNotConfigured means no Keycloak client is configured on this operator.
	ErrKeycloakNotConfigured = errors.New("keycloak client not configured")
)

// Budgets for the calls below. Each is a single HTTP round-trip (Vault,
// Keycloak) or a bounded intra-vcluster read (the reviewer token), not a poll
// loop — the reconcile loop supplies its own retry through requeue.
const (
	vaultCheckBudget   = 10 * time.Second
	vaultSetupBudget   = 30 * time.Second
	vaultWebhookBudget = 5 * time.Second

	// vaultReviewerTokenTTL matches the long-lived token the handlers' goroutine
	// requests today: a token this backend never rotates on its own.
	vaultReviewerTokenTTL = 876000 * time.Hour
)

// vaultAuthPath is the one place this string is built on the operator side —
// the vault.Client itself builds the same string again internally, but no
// third place should ever need to.
func vaultAuthPath(name, env string) string {
	return "kubernetes-vcluster-" + name + "-" + env
}

// VaultAuthConfigured asks Vault directly whether the Kubernetes auth backend
// for this vcluster already exists. This is the fact the operator reconciles
// from — never a flag it wrote itself on a previous pass (crd-vcluster.md
// §4.4's "le contrôleur ne relit donc aucune étape : il redemande au cluster",
// applied here to Vault instead of the cluster).
func (s *Service) VaultAuthConfigured(ctx context.Context, name, env string) (bool, error) {
	if s.vault == nil {
		return false, ErrVaultNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, vaultCheckBudget)
	defer cancel()
	return s.vault.AuthBackendExists(ctx, vaultAuthPath(name, env))
}

// VaultWebhookReady reports, from a single read (no polling), whether
// vault-webhook is deployed and healthy inside the vcluster — the
// prerequisite ConfigureVaultAuth needs before it can mint a reviewer token
// against the vault-webhook service account.
func (s *Service) VaultWebhookReady(ctx context.Context, name, env string) (bool, error) {
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return false, ErrK8sUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, vaultWebhookBudget)
	defer cancel()
	return k8s.VaultWebhookReady(ctx, name)
}

// ConfigureVaultAuth mints a reviewer token inside the vcluster and points
// Vault's Kubernetes auth backend at it. Safe to call again: enabling an
// already-enabled backend is a no-op in the Vault client, and reconfiguring
// just replaces the same config with the same values — but the caller only
// needs to, since VaultAuthConfigured already reports the backend as done
// once this has succeeded once.
func (s *Service) ConfigureVaultAuth(ctx context.Context, name, env string) error {
	if s.vault == nil {
		return ErrVaultNotConfigured
	}
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return ErrK8sUnavailable
	}

	ctx, cancel := context.WithTimeout(ctx, vaultSetupBudget)
	defer cancel()

	token, caCert, err := k8s.CreateVaultReviewerToken(ctx, name, vaultReviewerTokenTTL)
	if err != nil {
		return fmt.Errorf("génération du token reviewer : %w", err)
	}
	if err := s.vault.SetupVClusterAuth(ctx, name, env, s.vclusterAPIHost(name, env), caCert, token); err != nil {
		return fmt.Errorf("configuration du backend Vault : %w", err)
	}
	return nil
}

// vclusterAPIHost mirrors the "name.api.domain" derivation already used by
// the generator and the dashboard — recomputed here rather than stored, so a
// base domain change never leaves a stale value behind (crd-vcluster.md §2.2:
// nothing the generator derives belongs in spec or status).
func (s *Service) vclusterAPIHost(name, env string) string {
	domain := s.cfg.BaseDomainProd
	if env == "preprod" {
		domain = s.cfg.BaseDomainPreprod
	}
	return "https://" + name + ".api." + domain
}

// EnsureKeycloakClient creates the ArgoCD OIDC client for this vcluster+cell
// if it does not already exist. CreateArgoCDClients checks Keycloak for it
// itself before creating anything, so calling this on every reconcile pass is
// exactly the "ask the system, do not trust a flag" discipline the operator
// is built on — and it is what closes the asymmetry with DeleteArgoCDClients,
// already called by the finalizer.
//
// scope is a single cell name ("preprod" or "prod" today), not the
// "preprod"/"prod"/"both" scope service.Create passes: each cell's operator
// only ever owns its own client.
func (s *Service) EnsureKeycloakClient(name, env string) error {
	if s.keycloak == nil {
		return ErrKeycloakNotConfigured
	}
	return s.keycloak.CreateArgoCDClients(name, env)
}
