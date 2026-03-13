package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	_ "time/tzdata" // embed timezone DB for Europe/Paris in Docker containers without tzdata

	"golang.org/x/time/rate"

	"github.com/gmalfray/vcluster-manager/internal/argocd"
	"github.com/gmalfray/vcluster-manager/internal/auth"
	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/github"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/handlers"
	"github.com/gmalfray/vcluster-manager/internal/helmcharts"
	"github.com/gmalfray/vcluster-manager/internal/keycloak"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/metrics"
	"github.com/gmalfray/vcluster-manager/internal/notify"
	"github.com/gmalfray/vcluster-manager/internal/rancher"
	"github.com/gmalfray/vcluster-manager/internal/vault"
	"github.com/gmalfray/vcluster-manager/internal/version"
)

// splitCSV splits a comma-separated string into trimmed, non-empty tokens.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	ctx := context.Background()

	log.Printf("vCluster Manager %s starting", version.Version)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Configure admin groups from env (ADMIN_GROUPS).
	auth.SetAdminGroups(splitCSV(cfg.AdminGroups))

	// Parser (reads fluxprod repo via GitLab API)
	parser := gitops.NewParser()

	// GitLab client
	gl, err := gitops.NewGitLabClient(gitops.GitLabClientConfig{
		URL:                  cfg.GitLabURL,
		Token:                cfg.GitLabToken,
		ProjectID:            cfg.GitLabProjectID,
		ArgoCDGroupID:        cfg.GitLabGroupID,
		ArgoCDPath:           cfg.GitLabArgoCDPath,
		FluxDeployKeyID:      cfg.FluxCDDeployKey,
		DomainPreprod:        cfg.BaseDomainPreprod,
		DomainProd:           cfg.BaseDomainProd,
		VaultAddr:            cfg.VaultAddr,
		GitLabSSHURL:         cfg.GitLabSSHURL,
		ClustersPath:         cfg.FluxprodClustersPath,
		VaultKVArgoCDRootApps: cfg.VaultKVArgoCDRootApps,
		VaultKVArgoCDRepo:    cfg.VaultKVArgoCDRepo,
	})
	if err != nil {
		log.Fatalf("Failed to create GitLab client: %v", err)
	}
	parser.SetGitLabClient(gl)
	log.Println("GitLab client initialized")

	// Keycloak client (nil if credentials not set)
	var kc *keycloak.Client
	if cfg.KeycloakClientID != "" && cfg.KeycloakClientSecret != "" {
		kc = keycloak.NewClient(cfg.KeycloakURL, cfg.KeycloakRealm, cfg.KeycloakClientID, cfg.KeycloakClientSecret, cfg.BaseDomainPreprod, cfg.BaseDomainProd)
		log.Println("Keycloak client initialized (client_credentials)")
	} else {
		log.Printf("Keycloak client NOT initialized: missing credentials (ID=%q, Secret set?=%v)", cfg.KeycloakClientID, cfg.KeycloakClientSecret != "")
	}

	// Kubernetes status clients (one per env, optional)
	k8sClients := make(map[string]*kubernetes.StatusClient)

	// Helper to create a client for an env
	initK8sClient := func(env, kubeconfigPath, sshTunnel string) {
		if kubeconfigPath == "" {
			return
		}
		if sshTunnel != "" {
			client, tunnel, err := kubernetes.NewStatusClientWithTunnel(kubeconfigPath, sshTunnel, cfg.SSHKeyPath)
			if err != nil {
				log.Printf("K8s client for %s (SSH tunnel) failed: %v", env, err)
				return
			}
			_ = tunnel // tunnel stays open for the lifetime of the process
			k8sClients[env] = client
			log.Printf("K8s client for %s initialized (kubeconfig+SSH via %s)", env, sshTunnel)
		} else {
			client, err := kubernetes.NewStatusClient(kubeconfigPath)
			if err != nil {
				log.Printf("K8s client for %s failed: %v", env, err)
				return
			}
			k8sClients[env] = client
			log.Printf("K8s client for %s initialized (kubeconfig)", env)
		}
	}

	initK8sClient("preprod", cfg.KubeconfigPreprod, cfg.SSHTunnelPreprod)
	initK8sClient("prod", cfg.KubeconfigProd, cfg.SSHTunnelProd)

	// Fallback: if no per-env kubeconfig, use KUBECONFIG or in-cluster for all envs
	if len(k8sClients) == 0 {
		kubeconfig := os.Getenv("KUBECONFIG")
		client, err := kubernetes.NewStatusClient(kubeconfig)
		if err != nil {
			log.Printf("Kubernetes client not available: %v", err)
		} else {
			// Register the same client for both envs (backward compatibility)
			k8sClients["preprod"] = client
			k8sClients["prod"] = client
			log.Println("Kubernetes status client initialized (single cluster, both envs)")
		}
	}

	// GitHub releases client (for latest vcluster version)
	ghReleases := github.NewReleaseClient()
	log.Println("GitHub releases client initialized")

	// Helm charts updater (for updating vcluster chart in platform-helm-charts)
	var helmUpdater *helmcharts.Updater
	if cfg.HelmChartsProjectID != "" && cfg.GitLabToken != "" {
		helmGL, err := gitops.NewGitLabClient(gitops.GitLabClientConfig{
			URL:       cfg.GitLabURL,
			Token:     cfg.GitLabToken,
			ProjectID: cfg.HelmChartsProjectID,
		})
		if err != nil {
			log.Printf("Failed to create Helm charts GitLab client: %v", err)
		} else {
			helmUpdater = helmcharts.NewUpdater(helmGL, cfg.HelmChartsVClusterPath)
			log.Println("Helm charts updater initialized (project " + cfg.HelmChartsProjectID + ")")
		}
	}

	// ArgoCD updater (uses the fluxprod GitLab client, not helm charts)
	var argoUpdater *argocd.Updater
	if gl != nil {
		argoUpdater = argocd.NewUpdater(gl, cfg.FluxprodArgoCDKustPath)
		log.Println("ArgoCD updater initialized")
	}

	// Rancher client (optional, prod only)
	var rancherClient *rancher.Client
	if cfg.RancherToken != "" && cfg.RancherURL != "" {
		rancherClient = rancher.NewClient(cfg.RancherURL, cfg.RancherToken)
		log.Println("Rancher client initialized (" + cfg.RancherURL + ")")
	}

	// Vault client (optional — configures Kubernetes auth backends per vcluster)
	// AppRole (VAULT_ROLE_ID + VAULT_SECRET_ID) is preferred over a static VAULT_TOKEN.
	var vaultClient *vault.Client
	if cfg.VaultAddr != "" && cfg.VaultRoleID != "" && cfg.VaultSecretID != "" {
		var err error
		vaultClient, err = vault.NewClientWithAppRole(ctx, cfg.VaultAddr, cfg.VaultRoleID, cfg.VaultSecretID)
		if err != nil {
			log.Fatalf("Vault AppRole authentication failed: %v", err)
		}
		log.Println("Vault client initialized via AppRole (" + cfg.VaultAddr + ")")
	} else if cfg.VaultAddr != "" && cfg.VaultToken != "" {
		vaultClient = vault.NewClient(cfg.VaultAddr, cfg.VaultToken)
		log.Println("Vault client initialized via static token (" + cfg.VaultAddr + ") — consider switching to AppRole")
	} else {
		log.Println("Vault not configured (VAULT_ADDR + VAULT_ROLE_ID/VAULT_SECRET_ID missing) — skipping automatic Vault setup")
	}

	// Webhook notifier (optional)
	var webhookNotifier *notify.Notifier
	if cfg.WebhookURL != "" {
		webhookNotifier = notify.New(cfg.WebhookURL)
		log.Println("Webhook notifications enabled")
	}

	// Template directory
	templateDir := "web/templates"
	if dir := os.Getenv("TEMPLATE_DIR"); dir != "" {
		templateDir = dir
	}

	// Handlers
	h := handlers.New(cfg, parser, gl, kc, k8sClients, rancherClient, ghReleases, helmUpdater, argoUpdater, vaultClient, webhookNotifier, templateDir)

	// Auth setup
	var authMiddleware func(http.Handler) http.Handler
	var oidcAuth *auth.OIDCAuth
	var localAuth *auth.LocalAuth

	// OIDC auth (Keycloak)
	if cfg.OIDCClientID != "" {
		issuer := cfg.KeycloakURL + "/auth/realms/" + cfg.KeycloakRealm
		oidcAuth, err = auth.NewOIDCAuth(issuer, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCRedirectURL)
		if err != nil {
			log.Printf("OIDC initialization failed (continuing without): %v", err)
			oidcAuth = nil
		} else {
			http.HandleFunc("GET /auth/sso", oidcAuth.LoginHandler())
			http.HandleFunc("GET /auth/callback", oidcAuth.CallbackHandler())
			log.Println("OIDC authentication enabled")
		}
	}

	// Local auth (admin password)
	if cfg.AdminPassword != "" && cfg.JWTSecret != "" {
		localAuth = auth.NewLocalAuth(cfg.AdminPassword, cfg.JWTSecret, templateDir, oidcAuth != nil)
		http.HandleFunc("GET /auth/login", localAuth.LoginPageHandler())
		http.HandleFunc("POST /auth/local/login", localAuth.LoginHandler())
		log.Println("Local admin authentication enabled")
	} else if oidcAuth != nil {
		// OIDC only: login redirects to SSO directly
		http.HandleFunc("GET /auth/login", oidcAuth.LoginHandler())
	}

	// Logout route
	if oidcAuth != nil {
		http.HandleFunc("GET /auth/logout", oidcAuth.LogoutHandler())
	} else if localAuth != nil {
		http.HandleFunc("GET /auth/logout", func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{
				Name:     "session_token",
				Value:    "",
				Path:     "/",
				MaxAge:   -1,
				HttpOnly: true,
			})
			http.Redirect(w, r, "/auth/login", http.StatusTemporaryRedirect)
		})
	}

	// Build middleware
	if oidcAuth != nil || localAuth != nil {
		authMiddleware = auth.CombinedMiddleware(oidcAuth, localAuth)
	} else {
		authMiddleware = auth.NoopMiddleware
		log.Println("Running without authentication (dev mode)")
	}

	// Metrics endpoint (unauthenticated, scraping by Prometheus)
	http.Handle("GET /metrics", metrics.Handler())

	// Static files
	http.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	// Protected routes
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.Dashboard)
	mux.HandleFunc("GET /vclusters", h.ListVClusters)
	mux.HandleFunc("GET /vclusters/new", h.CreateForm)
	mux.HandleFunc("POST /vclusters/new", h.Create)
	mux.HandleFunc("GET /vclusters/{name}", h.Detail)
	mux.HandleFunc("POST /vclusters/{name}/settings", h.UpdateSettings)
	mux.HandleFunc("GET /vclusters/{name}/delete", h.DeleteConfirm)
	mux.HandleFunc("POST /vclusters/{name}/delete", h.Delete)
	mux.HandleFunc("GET /api/vclusters/{name}/kubeconfig", h.DownloadKubeconfig)
	mux.HandleFunc("POST /api/vclusters/{name}/create-repo", h.CreateAppManifestsRepo)
	mux.HandleFunc("POST /api/vclusters/{name}/create-prod-mr", h.CreateProdMR)
	mux.HandleFunc("GET /api/vclusters/{name}/status", h.StatusFragment)
	mux.HandleFunc("GET /api/vclusters/{name}/quotas", h.QuotaForm)
	mux.HandleFunc("GET /api/vclusters/{name}/rancher-status", h.RancherStatus)
	mux.HandleFunc("GET /api/vclusters/{name}/protection-status", h.ProtectionStatus)
	mux.HandleFunc("POST /api/vclusters/{name}/vault-setup-retry", h.RetryVaultSetup)
	mux.HandleFunc("POST /api/vclusters/{name}/pair-rancher", h.PairRancher)
	mux.HandleFunc("POST /api/vclusters/{name}/unpair-rancher", h.UnpairRancher)
	mux.HandleFunc("POST /api/vclusters/{name}/enable-protection", h.EnableProtection)
	mux.HandleFunc("POST /api/vclusters/{name}/disable-protection", h.DisableProtection)
	mux.HandleFunc("GET /api/vclusters/{name}/velero/backups", h.VeleroBackupList)
	mux.HandleFunc("GET /api/vclusters/{name}/velero/backups/{backup}/content", h.VeleroBackupContent)
	mux.HandleFunc("POST /api/vclusters/{name}/velero/backup", h.TriggerVeleroBackup)
	mux.HandleFunc("DELETE /api/vclusters/{name}/velero/backups/{backup}", h.DeleteVeleroBackup)
	mux.HandleFunc("POST /api/vclusters/{name}/velero/restore", h.CreateVeleroRestore)
	mux.HandleFunc("GET /api/vclusters/{name}/velero/restore/{restore}/status", h.VeleroRestoreStatus)
	mux.HandleFunc("GET /api/vclusters/{name}/apps", h.ListApps)
	mux.HandleFunc("POST /api/vclusters/{name}/apps/migrate", h.MigrateApp)
	mux.HandleFunc("POST /api/chart/update", h.UpdateChart)
	mux.HandleFunc("POST /api/chart/k8s-version", h.UpdateK8sVersion)
	mux.HandleFunc("POST /api/argocd/update", h.UpdateArgoCDVersion)
	mux.HandleFunc("GET /config", h.ClusterConfig)
	mux.HandleFunc("POST /config/{env}", h.UpdateClusterConfig)
	mux.HandleFunc("POST /config/velero", h.UpdateVeleroConfig)
	mux.HandleFunc("GET /api/clusters/{env}/health", h.ClusterHealth)
	mux.HandleFunc("GET /api/dashboard/flux-summary", h.FluxSummaryFragment)

	rateLimiter := auth.NewRateLimiter(rate.Limit(20), 50)
	http.Handle("/", metrics.Middleware(rateLimiter.Middleware(authMiddleware(auth.CSRFMiddleware(mux)))))

	log.Printf("Starting server on %s", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

