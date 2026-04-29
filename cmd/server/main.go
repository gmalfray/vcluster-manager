package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
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

// parseLogLevel reads LOG_LEVEL (debug|info|warn|error). Defaults to info.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// initLogger wires slog as the default logger and bridges the standard log
// package through it so existing log.Printf call sites flow into the same
// JSON pipeline. Per-call enrichment with structured fields is incremental
// (see TODO.md).
func initLogger() {
	level := parseLogLevel(os.Getenv("LOG_LEVEL"))
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
	// Route the legacy "log" package through slog at the configured level.
	slog.SetLogLoggerLevel(level)
	log.SetFlags(0)
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", "err", err)
		os.Exit(1)
	}
}

// run holds the actual server lifecycle so deferred cleanups (signal context,
// SSH tunnels, GitLab cache janitors) get a chance to execute before the
// process exits — os.Exit in main() would skip them.
func run() error {
	initLogger()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("vCluster Manager starting", "version", version.Version)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Configure admin groups from env (ADMIN_GROUPS).
	auth.SetAdminGroups(splitCSV(cfg.AdminGroups))

	// Parser (reads fluxprod repo via GitLab API)
	parser := gitops.NewParser()

	// GitLab client
	gl, err := gitops.NewGitLabClient(gitops.GitLabClientConfig{
		URL:                   cfg.GitLabURL,
		Token:                 cfg.GitLabToken,
		ProjectID:             cfg.GitLabProjectID,
		ArgoCDGroupID:         cfg.GitLabGroupID,
		ArgoCDPath:            cfg.GitLabArgoCDPath,
		FluxDeployKeyID:       int64(cfg.FluxCDDeployKey),
		DomainPreprod:         cfg.BaseDomainPreprod,
		DomainProd:            cfg.BaseDomainProd,
		VaultAddr:             cfg.VaultAddr,
		GitLabSSHURL:          cfg.GitLabSSHURL,
		ClustersPath:          cfg.FluxprodClustersPath,
		VaultKVArgoCDRootApps: cfg.VaultKVArgoCDRootApps,
		VaultKVArgoCDRepo:     cfg.VaultKVArgoCDRepo,
	})
	if err != nil {
		return fmt.Errorf("creating GitLab client: %w", err)
	}
	parser.SetGitLabClient(gl)
	slog.Info("GitLab client initialized")

	// Keycloak client (nil if credentials not set)
	var kc *keycloak.Client
	if cfg.KeycloakClientID != "" && cfg.KeycloakClientSecret != "" {
		kc = keycloak.NewClient(cfg.KeycloakURL, cfg.KeycloakRealm, cfg.KeycloakClientID, cfg.KeycloakClientSecret, cfg.BaseDomainPreprod, cfg.BaseDomainProd)
		slog.Info("Keycloak client initialized", "auth", "client_credentials")
	} else {
		slog.Warn("Keycloak client not initialized: missing credentials",
			"client_id", cfg.KeycloakClientID,
			"secret_set", cfg.KeycloakClientSecret != "")
	}

	// Kubernetes status clients (one per env, optional)
	k8sClients := make(map[string]*kubernetes.StatusClient)
	var tunnels []*kubernetes.SSHTunnel

	// Helper to create a client for an env
	initK8sClient := func(env, kubeconfigPath, sshTunnel string) {
		if kubeconfigPath == "" {
			return
		}
		if sshTunnel != "" {
			client, tunnel, err := kubernetes.NewStatusClientWithTunnel(kubeconfigPath, sshTunnel, cfg.SSHKeyPath)
			if err != nil {
				slog.Error("k8s client init failed", "env", env, "via", "ssh-tunnel", "err", err)
				return
			}
			tunnels = append(tunnels, tunnel)
			k8sClients[env] = client
			slog.Info("k8s client initialized", "env", env, "via", "kubeconfig+ssh", "tunnel", sshTunnel)
		} else {
			client, err := kubernetes.NewStatusClient(kubeconfigPath)
			if err != nil {
				slog.Error("k8s client init failed", "env", env, "via", "kubeconfig", "err", err)
				return
			}
			k8sClients[env] = client
			slog.Info("k8s client initialized", "env", env, "via", "kubeconfig")
		}
	}

	initK8sClient("preprod", cfg.KubeconfigPreprod, cfg.SSHTunnelPreprod)
	initK8sClient("prod", cfg.KubeconfigProd, cfg.SSHTunnelProd)

	// Fallback: if no per-env kubeconfig, use KUBECONFIG or in-cluster for all envs
	if len(k8sClients) == 0 {
		kubeconfig := os.Getenv("KUBECONFIG")
		client, err := kubernetes.NewStatusClient(kubeconfig)
		if err != nil {
			slog.Warn("k8s client not available", "err", err)
		} else {
			// Register the same client for both envs (backward compatibility)
			k8sClients["preprod"] = client
			k8sClients["prod"] = client
			slog.Info("k8s client initialized", "scope", "single-cluster-both-envs")
		}
	}

	// GitHub releases client (for latest vcluster version)
	ghReleases := github.NewReleaseClient()
	slog.Info("GitHub releases client initialized")

	// Helm charts updater (for updating vcluster chart in platform-helm-charts)
	var helmUpdater *helmcharts.Updater
	var helmGL *gitops.GitLabClient
	if cfg.HelmChartsProjectID != "" && cfg.GitLabToken != "" {
		var err error
		helmGL, err = gitops.NewGitLabClient(gitops.GitLabClientConfig{
			URL:       cfg.GitLabURL,
			Token:     cfg.GitLabToken,
			ProjectID: cfg.HelmChartsProjectID,
		})
		if err != nil {
			slog.Error("helm charts GitLab client init failed", "err", err)
		} else {
			helmUpdater = helmcharts.NewUpdater(helmGL, cfg.HelmChartsVClusterPath)
			slog.Info("helm charts updater initialized", "project", cfg.HelmChartsProjectID)
		}
	}

	// ArgoCD updater (uses the fluxprod GitLab client, not helm charts)
	var argoUpdater *argocd.Updater
	if gl != nil {
		argoUpdater = argocd.NewUpdater(gl, cfg.FluxprodArgoCDKustPath)
		slog.Info("ArgoCD updater initialized")
	}

	// Rancher client (optional, prod only)
	var rancherClient *rancher.Client
	if cfg.RancherToken != "" && cfg.RancherURL != "" {
		rancherClient = rancher.NewClient(cfg.RancherURL, cfg.RancherToken)
		slog.Info("Rancher client initialized", "url", cfg.RancherURL)
	}

	// Vault client (optional — configures Kubernetes auth backends per vcluster)
	// AppRole (VAULT_ROLE_ID + VAULT_SECRET_ID) is preferred over a static VAULT_TOKEN.
	var vaultClient *vault.Client
	if cfg.VaultAddr != "" && cfg.VaultRoleID != "" && cfg.VaultSecretID != "" {
		var err error
		vaultClient, err = vault.NewClientWithAppRole(ctx, cfg.VaultAddr, cfg.VaultRoleID, cfg.VaultSecretID)
		if err != nil {
			return fmt.Errorf("vault AppRole authentication: %w", err)
		}
		slog.Info("vault client initialized", "auth", "approle", "addr", cfg.VaultAddr)
	} else if cfg.VaultAddr != "" && cfg.VaultToken != "" {
		vaultClient = vault.NewClient(cfg.VaultAddr, cfg.VaultToken)
		slog.Warn("vault client initialized with static token; consider switching to AppRole",
			"auth", "static-token", "addr", cfg.VaultAddr)
	} else {
		slog.Info("vault not configured; skipping automatic Vault setup",
			"hint", "set VAULT_ADDR + VAULT_ROLE_ID/VAULT_SECRET_ID")
	}

	// Webhook notifier (optional)
	var webhookNotifier *notify.Notifier
	if cfg.WebhookURL != "" {
		webhookNotifier = notify.New(cfg.WebhookURL)
		slog.Info("webhook notifications enabled")
	}

	// Template directory
	templateDir := "web/templates"
	if dir := os.Getenv("TEMPLATE_DIR"); dir != "" {
		templateDir = dir
	}

	// Handlers
	h := handlers.New(handlers.Deps{
		Config:         cfg,
		Parser:         parser,
		GitLab:         gl,
		Keycloak:       kc,
		K8sClients:     k8sClients,
		Rancher:        rancherClient,
		GitHubReleases: ghReleases,
		HelmUpdater:    helmUpdater,
		ArgoCDUpdater:  argoUpdater,
		Vault:          vaultClient,
		Notifier:       webhookNotifier,
		TemplateDir:    templateDir,
	})

	// Auth setup
	var authMiddleware func(http.Handler) http.Handler
	var oidcAuth *auth.OIDCAuth
	var localAuth *auth.LocalAuth

	// OIDC auth (Keycloak)
	if cfg.OIDCClientID != "" {
		issuer := cfg.KeycloakURL + "/auth/realms/" + cfg.KeycloakRealm
		oidcAuth, err = auth.NewOIDCAuth(issuer, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCRedirectURL)
		if err != nil {
			slog.Error("OIDC initialization failed; continuing without", "err", err)
			oidcAuth = nil
		} else {
			http.HandleFunc("GET /auth/sso", oidcAuth.LoginHandler())
			http.HandleFunc("GET /auth/callback", oidcAuth.CallbackHandler())
			slog.Info("OIDC authentication enabled")
		}
	}

	// Local auth (admin password)
	if cfg.AdminPassword != "" && cfg.JWTSecret != "" {
		localAuth = auth.NewLocalAuth(cfg.AdminPassword, cfg.JWTSecret, templateDir, oidcAuth != nil)
		http.HandleFunc("GET /auth/login", localAuth.LoginPageHandler())
		http.HandleFunc("POST /auth/local/login", localAuth.LoginHandler())
		slog.Info("local admin authentication enabled")
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
		slog.Warn("running without authentication", "mode", "dev")
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

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server starting", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining requests")
	case err := <-serverErr:
		return fmt.Errorf("server failed: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "err", err)
	}

	for _, t := range tunnels {
		if err := t.Close(); err != nil {
			slog.Warn("SSH tunnel close error", "err", err)
		}
	}

	// Stop the cache janitors so they don't outlive the process briefly.
	gl.Close()
	if helmGL != nil {
		helmGL.Close()
	}

	slog.Info("server stopped cleanly")
	return nil
}
