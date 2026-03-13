package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
)

type Config struct {
	// Server
	ListenAddr string

	// GitLab
	GitLabURL        string
	GitLabToken      string
	GitLabProjectID  string
	GitLabGroupID    string
	FluxCDDeployKey  int

	// Keycloak (Service Account — client_credentials)
	KeycloakURL          string
	KeycloakRealm        string
	KeycloakClientID     string
	KeycloakClientSecret string

	// OIDC (app auth)
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string

	// Local auth
	AdminPassword string
	JWTSecret     string

	// Helm charts GitLab project
	HelmChartsProjectID string

	// Multi-cluster Kubernetes
	KubeconfigPreprod    string // KUBECONFIG_PREPROD — path to preprod cluster kubeconfig
	KubeconfigProd       string // KUBECONFIG_PROD — path to prod cluster kubeconfig
	ClusterLabelPreprod  string // CLUSTER_LABEL_PREPROD — display name for preprod host cluster
	ClusterLabelProd     string // CLUSTER_LABEL_PROD — display name for prod host cluster

	// SSH tunnel (optional, for restricted networks)
	SSHTunnelPreprod string // SSH_TUNNEL_PREPROD — e.g. "user@bastion:22226"
	SSHTunnelProd    string // SSH_TUNNEL_PROD
	SSHKeyPath       string // SSH_KEY_PATH — default: /root/.ssh/id_rsa

	// Rancher (optional)
	RancherURL            string // RANCHER_URL — e.g. "https://rancher.example.com"
	RancherToken          string // RANCHER_TOKEN — Bearer token for Rancher API
	RancherEnabledPreprod bool   // RANCHER_ENABLED_PREPROD — enable Rancher pairing for preprod
	RancherEnabledProd    bool   // RANCHER_ENABLED_PROD — enable Rancher pairing for prod

	// Vault (optional)
	VaultAddr     string // VAULT_ADDR — e.g. "https://vault.example.com"
	VaultRoleID   string // VAULT_ROLE_ID — AppRole role_id (preferred)
	VaultSecretID string // VAULT_SECRET_ID — AppRole secret_id (preferred)
	VaultToken    string // VAULT_TOKEN — static token fallback (deprecated, use AppRole)

	// GitOps domains (used to build vcluster hostnames and ArgoCD URLs)
	BaseDomainPreprod string // BASE_DOMAIN_PREPROD — e.g. "preprod.example.com"
	BaseDomainProd    string // BASE_DOMAIN_PROD — e.g. "example.com"

	// GitLab SSH (used in GitOps templates to build app-manifests SSH URLs)
	GitLabSSHURL     string // GITLAB_SSH_URL — e.g. "ssh://git@gitlab.example.com:22"
	GitLabArgoCDPath string // GITLAB_ARGOCD_PATH — namespace path of ArgoCD repos, e.g. "ops/argocd"

	// TLS secrets for ArgoCD ingress (pre-existing wildcard secrets in host clusters)
	TLSSecretPreprod string // TLS_SECRET_PREPROD — e.g. "wildcard-preprod-example-com-tls"
	TLSSecretProd    string // TLS_SECRET_PROD — e.g. "wildcard-example-com-tls"

	// Notifications (optional)
	WebhookURL string // WEBHOOK_URL — Slack/Mattermost/generic webhook for event notifications

	// Admin access — OIDC groups with write access (comma-separated)
	AdminGroups string // ADMIN_GROUPS — default: "platform-admins,ops"

	// GitOps repo structure (paths within the fluxprod repo)
	FluxprodClustersPath   string // FLUXPROD_CLUSTERS_PATH — prefix for cluster dirs, default: "clusters"
	FluxprodArgoCDKustPath string // FLUXPROD_ARGOCD_KUST_PATH — path to global ArgoCD kustomization.yaml, default: "lib/tenant-template/argocd/base/kustomization.yaml"

	// Helm charts repo structure
	HelmChartsVClusterPath string // HELM_CHARTS_VCLUSTER_PATH — path to vcluster chart dir in helm-charts repo, default: "charts/vcluster"

	// Vault KV paths (used in generated app-manifests README)
	VaultKVArgoCDRootApps string // VAULT_KV_ARGOCD_ROOTAPPS — Vault KV path for ArgoCD root deploy key
	VaultKVArgoCDRepo     string // VAULT_KV_ARGOCD_REPO — Vault KV path for ArgoCD repo deploy key

	// Default RBAC group added to ArgoCD policy when creating a vcluster without explicit groups
	DefaultRBACGroup string // DEFAULT_RBAC_GROUP — default: "it"

	// vCluster defaults (used when creating a vcluster without explicit values)
	DefaultCPU          string // DEFAULT_CPU — default CPU quota, e.g. "8"
	DefaultMemory       string // DEFAULT_MEMORY — default memory quota, e.g. "32Gi"
	DefaultStorage      string // DEFAULT_STORAGE — default storage quota, e.g. "500Gi"
	VeleroTimezone      string // VELERO_TIMEZONE — cron timezone for Velero, e.g. "Europe/Paris"
	VeleroDefaultTTL    string // VELERO_DEFAULT_TTL — default backup retention, e.g. "720h0m0s" (30d)
	VeleroNamespace     string // VELERO_NAMESPACE — namespace where Velero is installed, e.g. "velero-system"
	VeleroS3URL         string // VELERO_S3_URL — S3 endpoint for Velero BSL, e.g. "https://s3.example.com"
	VeleroBucketPreprod string // VELERO_BUCKET_PREPROD — S3 bucket for preprod Velero backups
	VeleroBucketProd    string // VELERO_BUCKET_PROD — S3 bucket for prod Velero backups
	VClusterPodSecurity string // VCLUSTER_POD_SECURITY — podSecurityStandard, e.g. "privileged"
	ArgoCDDefaultPolicy string // ARGOCD_DEFAULT_POLICY — default RBAC policy, e.g. "role:readonly"

	// Runtime settings (persisted via stateBackend)
	mu      sync.RWMutex
	dataDir string
	backend stateBackend
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:          os.Getenv("LISTEN_ADDR"),
		GitLabURL:           os.Getenv("GITLAB_URL"),
		GitLabToken:         os.Getenv("GITLAB_TOKEN"),
		GitLabProjectID:     os.Getenv("GITLAB_PROJECT_ID"),
		GitLabGroupID:       os.Getenv("GITLAB_ARGOCD_GROUP_ID"),
		FluxCDDeployKey:     getIntEnv("FLUXCD_DEPLOY_KEY_ID"),
		GitLabSSHURL:        os.Getenv("GITLAB_SSH_URL"),
		GitLabArgoCDPath:    os.Getenv("GITLAB_ARGOCD_PATH"),
		KeycloakURL:         os.Getenv("KEYCLOAK_URL"),
		KeycloakRealm:       os.Getenv("KEYCLOAK_REALM"),
		KeycloakClientID:    os.Getenv("KEYCLOAK_CLIENT_ID"),
		KeycloakClientSecret: os.Getenv("KEYCLOAK_CLIENT_SECRET"),
		OIDCClientID:        os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:    os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:     os.Getenv("OIDC_REDIRECT_URL"),
		HelmChartsProjectID: os.Getenv("GITLAB_HELM_PROJECT_ID"),
		AdminPassword:       os.Getenv("ADMIN_PASSWORD"),
		JWTSecret:           os.Getenv("JWT_SECRET"),
		KubeconfigPreprod:   os.Getenv("KUBECONFIG_PREPROD"),
		KubeconfigProd:      os.Getenv("KUBECONFIG_PROD"),
		SSHTunnelPreprod:    os.Getenv("SSH_TUNNEL_PREPROD"),
		SSHTunnelProd:       os.Getenv("SSH_TUNNEL_PROD"),
		SSHKeyPath:          os.Getenv("SSH_KEY_PATH"),
		ClusterLabelPreprod: os.Getenv("CLUSTER_LABEL_PREPROD"),
		ClusterLabelProd:    os.Getenv("CLUSTER_LABEL_PROD"),
		RancherURL:            os.Getenv("RANCHER_URL"),
		RancherToken:          os.Getenv("RANCHER_TOKEN"),
		RancherEnabledPreprod: os.Getenv("RANCHER_ENABLED_PREPROD") == "true",
		RancherEnabledProd:    os.Getenv("RANCHER_ENABLED_PROD") == "true",
		VaultAddr:           os.Getenv("VAULT_ADDR"),
		VaultRoleID:         os.Getenv("VAULT_ROLE_ID"),
		VaultSecretID:       os.Getenv("VAULT_SECRET_ID"),
		VaultToken:          os.Getenv("VAULT_TOKEN"),
		BaseDomainPreprod:   os.Getenv("BASE_DOMAIN_PREPROD"),
		BaseDomainProd:      os.Getenv("BASE_DOMAIN_PROD"),
		TLSSecretPreprod:    os.Getenv("TLS_SECRET_PREPROD"),
		TLSSecretProd:       os.Getenv("TLS_SECRET_PROD"),
		DefaultCPU:          os.Getenv("DEFAULT_CPU"),
		DefaultMemory:       os.Getenv("DEFAULT_MEMORY"),
		DefaultStorage:      os.Getenv("DEFAULT_STORAGE"),
		VeleroTimezone:      os.Getenv("VELERO_TIMEZONE"),
		VeleroDefaultTTL:    getEnvOrDefault("VELERO_DEFAULT_TTL", "720h0m0s"),
		VeleroNamespace:     getEnvOrDefault("VELERO_NAMESPACE", "velero-system"),
		VeleroS3URL:         os.Getenv("VELERO_S3_URL"),
		VeleroBucketPreprod: os.Getenv("VELERO_BUCKET_PREPROD"),
		VeleroBucketProd:    os.Getenv("VELERO_BUCKET_PROD"),
		VClusterPodSecurity:    os.Getenv("VCLUSTER_POD_SECURITY"),
		ArgoCDDefaultPolicy:    os.Getenv("ARGOCD_DEFAULT_POLICY"),
		WebhookURL:             os.Getenv("WEBHOOK_URL"),
		AdminGroups:            getEnvOrDefault("ADMIN_GROUPS", "platform-admins,ops"),
		FluxprodClustersPath:   getEnvOrDefault("FLUXPROD_CLUSTERS_PATH", "clusters"),
		FluxprodArgoCDKustPath: getEnvOrDefault("FLUXPROD_ARGOCD_KUST_PATH", "lib/tenant-template/argocd/base/kustomization.yaml"),
		HelmChartsVClusterPath: getEnvOrDefault("HELM_CHARTS_VCLUSTER_PATH", "charts/vcluster"),
		VaultKVArgoCDRootApps:  getEnvOrDefault("VAULT_KV_ARGOCD_ROOTAPPS", "secret/data/vcluster/argocd/rootapps"),
		VaultKVArgoCDRepo:      getEnvOrDefault("VAULT_KV_ARGOCD_REPO", "secret/data/vcluster/argocd/repo"),
		DefaultRBACGroup:       getEnvOrDefault("DEFAULT_RBAC_GROUP", "developers"),
	}

	if c.GitLabToken == "" {
		return nil, fmt.Errorf("GITLAB_TOKEN is required")
	}

	c.dataDir = os.Getenv("DATA_DIR")
	if c.dataDir == "" {
		c.dataDir = "data"
	}

	switch os.Getenv("STATE_BACKEND") {
	case "configmap":
		b, err := newConfigMapBackend()
		if err != nil {
			return nil, fmt.Errorf("configmap state backend: %w", err)
		}
		c.backend = b
		log.Println("State backend: Kubernetes ConfigMap (vcluster-manager-state)")
	default:
		c.backend = &fileBackend{dataDir: c.dataDir}
		log.Printf("State backend: local files (%s/)", c.dataDir)
	}

	c.loadPersistedSettings()

	return c, nil
}

// DataDir returns the path to the persistent data directory.
func (c *Config) DataDir() string {
	return c.dataDir
}

// ClusterSettings holds the per-env cluster connectivity config.
type ClusterSettings struct {
	Label      string `json:"label,omitempty"`
	Kubeconfig string `json:"kubeconfig,omitempty"` // path to kubeconfig file in data dir
	SSHTunnel  string `json:"ssh_tunnel,omitempty"` // e.g. "user@bastion:22226"
	SSHKeyPath string `json:"ssh_key_path,omitempty"` // path to SSH key file in data dir
}

// persistedSettings represents the settings saved to disk.
type persistedSettings struct {
	Clusters            map[string]*ClusterSettings `json:"clusters,omitempty"` // key: "preprod", "prod"
	VeleroDefaultTTL    string                      `json:"velero_default_ttl,omitempty"`
	VeleroS3URL         string                      `json:"velero_s3_url,omitempty"`
	VeleroBucketPreprod string                      `json:"velero_bucket_preprod,omitempty"`
	VeleroBucketProd    string                      `json:"velero_bucket_prod,omitempty"`
}

// loadPersistedSettings reads saved settings and applies them (overrides env var defaults).
func (c *Config) loadPersistedSettings() {
	data, err := c.backend.readSettings()
	if err != nil {
		return // no settings yet, use defaults
	}
	var s persistedSettings
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("Warning: could not parse settings: %v", err)
		return
	}
	if s.VeleroDefaultTTL != "" {
		c.VeleroDefaultTTL = s.VeleroDefaultTTL
	}
	if s.VeleroS3URL != "" {
		c.VeleroS3URL = s.VeleroS3URL
	}
	if s.VeleroBucketPreprod != "" {
		c.VeleroBucketPreprod = s.VeleroBucketPreprod
	}
	if s.VeleroBucketProd != "" {
		c.VeleroBucketProd = s.VeleroBucketProd
	}
	for env, cs := range s.Clusters {
		if cs.Label != "" {
			switch env {
			case "preprod":
				c.ClusterLabelPreprod = cs.Label
			case "prod":
				c.ClusterLabelProd = cs.Label
			}
		}
		if cs.Kubeconfig != "" {
			switch env {
			case "preprod":
				c.KubeconfigPreprod = cs.Kubeconfig
			case "prod":
				c.KubeconfigProd = cs.Kubeconfig
			}
		}
		if cs.SSHTunnel != "" {
			switch env {
			case "preprod":
				c.SSHTunnelPreprod = cs.SSHTunnel
			case "prod":
				c.SSHTunnelProd = cs.SSHTunnel
			}
		}
		if cs.SSHKeyPath != "" {
			c.SSHKeyPath = cs.SSHKeyPath
		}
	}
}

// SetVeleroConfig updates the global Velero settings and persists them.
func (c *Config) SetVeleroConfig(defaultTTL, s3URL, bucketPreprod, bucketProd string) {
	c.mu.Lock()
	if defaultTTL != "" {
		c.VeleroDefaultTTL = defaultTTL
	}
	if s3URL != "" {
		c.VeleroS3URL = s3URL
	}
	if bucketPreprod != "" {
		c.VeleroBucketPreprod = bucketPreprod
	}
	if bucketProd != "" {
		c.VeleroBucketProd = bucketProd
	}
	c.mu.Unlock()
	if err := c.savePersistedSettings(); err != nil {
		log.Printf("Warning: could not save settings: %v", err)
	}
}

// savePersistedSettings persists current settings via the backend.
func (c *Config) savePersistedSettings() error {
	s := persistedSettings{
		VeleroDefaultTTL:    c.VeleroDefaultTTL,
		VeleroS3URL:         c.VeleroS3URL,
		VeleroBucketPreprod: c.VeleroBucketPreprod,
		VeleroBucketProd:    c.VeleroBucketProd,
		Clusters: map[string]*ClusterSettings{
			"preprod": {
				Label:      c.ClusterLabelPreprod,
				Kubeconfig: c.KubeconfigPreprod,
				SSHTunnel:  c.SSHTunnelPreprod,
				SSHKeyPath: c.SSHKeyPath,
			},
			"prod": {
				Label:      c.ClusterLabelProd,
				Kubeconfig: c.KubeconfigProd,
				SSHTunnel:  c.SSHTunnelProd,
				SSHKeyPath: c.SSHKeyPath,
			},
		},
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}
	return c.backend.writeSettings(data)
}

// ClusterLabel returns the display label for the given environment.
func (c *Config) ClusterLabel(env string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch env {
	case "preprod":
		return c.ClusterLabelPreprod
	case "prod":
		return c.ClusterLabelProd
	default:
		return env
	}
}

// SetClusterLabel updates the display label for the given environment and persists it.
func (c *Config) SetClusterLabel(env, label string) {
	c.mu.Lock()
	switch env {
	case "preprod":
		c.ClusterLabelPreprod = label
	case "prod":
		c.ClusterLabelProd = label
	}
	c.mu.Unlock()

	if err := c.savePersistedSettings(); err != nil {
		log.Printf("Warning: could not save settings: %v", err)
	}
}

// SetClusterConfig updates cluster connectivity settings and persists them.
func (c *Config) SetClusterConfig(env, kubeconfigPath, sshTunnel, sshKeyPath string) {
	c.mu.Lock()
	switch env {
	case "preprod":
		c.KubeconfigPreprod = kubeconfigPath
		c.SSHTunnelPreprod = sshTunnel
	case "prod":
		c.KubeconfigProd = kubeconfigPath
		c.SSHTunnelProd = sshTunnel
	}
	if sshKeyPath != "" {
		c.SSHKeyPath = sshKeyPath
	}
	c.mu.Unlock()

	if err := c.savePersistedSettings(); err != nil {
		log.Printf("Warning: could not save settings: %v", err)
	}
}

// GetClusterConfig returns the current cluster connectivity settings for an env.
func (c *Config) GetClusterConfig(env string) (kubeconfigPath, sshTunnel, sshKeyPath string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch env {
	case "preprod":
		return c.KubeconfigPreprod, c.SSHTunnelPreprod, c.SSHKeyPath
	case "prod":
		return c.KubeconfigProd, c.SSHTunnelProd, c.SSHKeyPath
	}
	return "", "", c.SSHKeyPath
}

// RancherEnabledForEnv returns true if Rancher is enabled for the given environment.
func (c *Config) RancherEnabledForEnv(env string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch env {
	case "preprod":
		return c.RancherEnabledPreprod
	case "prod":
		return c.RancherEnabledProd
	default:
		return false
	}
}

func getIntEnv(key string) int {
	var i int
	fmt.Sscanf(os.Getenv(key), "%d", &i)
	return i
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
