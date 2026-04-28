package handlers

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/argocd"
	"github.com/gmalfray/vcluster-manager/internal/auth"
	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/github"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/helmcharts"
	"github.com/gmalfray/vcluster-manager/internal/keycloak"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/notify"
	"github.com/gmalfray/vcluster-manager/internal/rancher"
	"github.com/gmalfray/vcluster-manager/internal/vault"
	"github.com/gmalfray/vcluster-manager/internal/version"
)

// migrationEntry tracks an in-progress app migration (expires after 15 minutes).
type migrationEntry struct {
	Source    string
	Target    string
	ExpiresAt time.Time
}

// vaultSetupState tracks the async Vault auth backend setup for a vcluster.
type vaultSetupState struct {
	Status    string // "waiting" | "configuring" | "done" | "error"
	Message   string // error message if Status == "error"
	UpdatedAt time.Time
}

type Handlers struct {
	cfg           *config.Config
	parser        *gitops.Parser
	generator     *gitops.Generator
	gitlab        *gitops.GitLabClient
	keycloak      *keycloak.Client
	k8sClients    map[string]*kubernetes.StatusClient // key: "preprod" or "prod"
	k8sClientsMu  sync.RWMutex
	rancher       *rancher.Client
	vault         *vault.Client
	ghReleases    *github.ReleaseClient
	helmUpdater   *helmcharts.Updater
	argocdUpdater *argocd.Updater
	pages         map[string]*template.Template
	partials      *template.Template
	templateDir   string
	migrations    map[string]migrationEntry // key: "env:vcName:appName"
	migrationsMu  sync.Mutex
	vaultStates   map[string]*vaultSetupState // key: "env/name"
	vaultStatesMu sync.RWMutex
	notifier      *notify.Notifier
}

func New(cfg *config.Config, parser *gitops.Parser, gl *gitops.GitLabClient, kc *keycloak.Client, k8sClients map[string]*kubernetes.StatusClient, rc *rancher.Client, ghReleases *github.ReleaseClient, helmUpdater *helmcharts.Updater, argoUpdater *argocd.Updater, vc *vault.Client, notifier *notify.Notifier, templateDir string) *Handlers {
	h := &Handlers{
		cfg:    cfg,
		parser: parser,
		generator: gitops.NewGenerator(gitops.GeneratorConfig{
			BaseDomainPreprod:   cfg.BaseDomainPreprod,
			BaseDomainProd:      cfg.BaseDomainProd,
			TLSSecretPreprod:    cfg.TLSSecretPreprod,
			TLSSecretProd:       cfg.TLSSecretProd,
			OIDCIssuer:          cfg.KeycloakURL + "/auth/realms/" + cfg.KeycloakRealm,
			GitLabSSHURL:        cfg.GitLabSSHURL,
			GitLabArgoCDPath:    cfg.GitLabArgoCDPath,
			DefaultCPU:          cfg.DefaultCPU,
			DefaultMemory:       cfg.DefaultMemory,
			DefaultStorage:      cfg.DefaultStorage,
			VeleroTimezone:      cfg.VeleroTimezone,
			VeleroDefaultTTL:    cfg.VeleroDefaultTTL,
			VClusterPodSecurity: cfg.VClusterPodSecurity,
			ArgoCDDefaultPolicy: cfg.ArgoCDDefaultPolicy,
		}),
		gitlab:        gl,
		keycloak:      kc,
		k8sClients:    k8sClients,
		rancher:       rc,
		vault:         vc,
		ghReleases:    ghReleases,
		helmUpdater:   helmUpdater,
		argocdUpdater: argoUpdater,
		pages:         make(map[string]*template.Template),
		templateDir:   templateDir,
		migrations:    make(map[string]migrationEntry),
		vaultStates:   make(map[string]*vaultSetupState),
		notifier:      notifier,
	}

	funcMap := template.FuncMap{
		"join":       strings.Join,
		"contains":   strings.Contains,
		"upper":      strings.ToUpper,
		"lower":      strings.ToLower,
		"trimPrefix": strings.TrimPrefix,
		"formatDate": func(s string) string {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return s
			}
			loc, err := time.LoadLocation("Europe/Paris")
			if err != nil {
				return t.Format("2006-01-02 15:04 UTC")
			}
			return t.In(loc).Format("2006-01-02 15:04")
		},
		"hasKey": func(m map[string]bool, key string) bool {
			if m == nil {
				return false
			}
			return m[key]
		},
	}

	layoutFile := filepath.Join(templateDir, "layout.html")
	partialFiles, _ := filepath.Glob(filepath.Join(templateDir, "partials", "*.html"))

	// Parse each page template with layout + partials
	pageFiles, _ := filepath.Glob(filepath.Join(templateDir, "*.html"))
	for _, pf := range pageFiles {
		name := filepath.Base(pf)
		if name == "layout.html" {
			continue
		}
		files := append([]string{layoutFile, pf}, partialFiles...)
		h.pages[name] = template.Must(
			template.New("").Funcs(funcMap).ParseFiles(files...),
		)
	}

	// Parse partials alone for HTMX fragment rendering
	h.partials = template.Must(
		template.New("").Funcs(funcMap).ParseFiles(partialFiles...),
	)

	go h.startVaultReconciler()
	go h.startCleaningReconciler()

	return h
}

// startVaultReconciler runs at startup: for every existing vcluster, checks whether the
// Vault auth backend is already configured. If yes, marks state as "done". If not,
// launches setupVaultAuthWhenReady to configure it. This recovers from restarts.
func (h *Handlers) startVaultReconciler() {
	if h.vault == nil {
		return
	}

	for _, env := range []string{"preprod", "prod"} {
		vclusters, err := h.parser.ListVClusters(env)
		if err != nil {
			slog.Warn("vault startup: could not list vclusters", "env", env, "err", err)
			continue
		}

		for _, vc := range vclusters {
			name := vc.Name
			env := env
			path := "kubernetes-vcluster-" + name + "-" + env

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			exists, err := h.vault.AuthBackendExists(ctx, path)
			cancel()

			if err != nil {
				slog.Warn("vault startup: checking backend failed, will retry", "env", env, "vcluster", name, "err", err)
				go h.setupVaultAuthWhenReady(name, env)
				continue
			}

			if exists {
				h.setVaultState(env, name, "done", "")
				slog.Info("vault startup: already configured", "env", env, "vcluster", name)
			} else {
				slog.Info("vault startup: backend missing, launching setup", "env", env, "vcluster", name)
				go h.setupVaultAuthWhenReady(name, env)
			}
		}
	}
}

// setVaultState updates the Vault setup state for a given vcluster.
func (h *Handlers) setVaultState(env, name, status, msg string) {
	h.vaultStatesMu.Lock()
	defer h.vaultStatesMu.Unlock()
	h.vaultStates[env+"/"+name] = &vaultSetupState{Status: status, Message: msg, UpdatedAt: time.Now()}
}

// getVaultState returns the current Vault setup state for a given vcluster, or nil if unknown.
func (h *Handlers) getVaultState(env, name string) *vaultSetupState {
	h.vaultStatesMu.RLock()
	defer h.vaultStatesMu.RUnlock()
	return h.vaultStates[env+"/"+name]
}

// k8sForEnv returns the StatusClient for the given environment, with fallback.
func (h *Handlers) k8sForEnv(env string) *kubernetes.StatusClient {
	h.k8sClientsMu.RLock()
	defer h.k8sClientsMu.RUnlock()

	if c, ok := h.k8sClients[env]; ok {
		return c
	}
	// Fallback: return any available client (backward compatibility)
	for _, c := range h.k8sClients {
		return c
	}
	return nil
}

// ClusterConfig shows the cluster configuration page (admin only).
func (h *Handlers) ClusterConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	type clusterInfo struct {
		Env          string
		Label        string
		Configured   bool
		ConfigSource string
		Kubeconfig   string
		SSHTarget    string
		SSHKeyPath   string
		APIServerURL string
	}

	var clusters []clusterInfo
	for _, env := range []string{"preprod", "prod"} {
		kubeconfig, sshTunnel, sshKey := h.cfg.GetClusterConfig(env)
		ci := clusterInfo{
			Env:        env,
			Label:      h.cfg.ClusterLabel(env),
			SSHTarget:  sshTunnel,
			SSHKeyPath: sshKey,
			Kubeconfig: kubeconfig,
		}
		if client, ok := h.k8sClients[env]; ok {
			ci.Configured = true
			ci.ConfigSource = client.ConfigSource()
			ci.APIServerURL = client.APIServerURL()
		}
		clusters = append(clusters, ci)
	}

	h.render(w, "cluster_config.html", map[string]interface{}{
		"Clusters": clusters,
		"User":     h.getUser(r),
		// GitLab
		"GitLabURL":        h.cfg.GitLabURL,
		"GitLabConfigured": h.cfg.GitLabToken != "",
		"GitLabProjectID":  h.cfg.GitLabProjectID,
		"GitLabGroupID":    h.cfg.GitLabGroupID,
		"FluxCDDeployKey":  h.cfg.FluxCDDeployKey,
		// Keycloak
		"KeycloakURL":        h.cfg.KeycloakURL,
		"KeycloakRealm":      h.cfg.KeycloakRealm,
		"KeycloakClientID":   h.cfg.KeycloakClientID,
		"KeycloakConfigured": h.cfg.KeycloakClientID != "" && h.cfg.KeycloakClientSecret != "",
		// OIDC
		"OIDCConfigured":  h.cfg.OIDCClientID != "",
		"OIDCClientID":    h.cfg.OIDCClientID,
		"OIDCRedirectURL": h.cfg.OIDCRedirectURL,
		// Helm Charts
		"HelmConfigured":      h.cfg.HelmChartsProjectID != "",
		"HelmChartsProjectID": h.cfg.HelmChartsProjectID,
		// Rancher
		"RancherConfigured":     h.rancher != nil,
		"RancherURL":            h.cfg.RancherURL,
		"RancherEnabledPreprod": h.cfg.RancherEnabledPreprod,
		"RancherEnabledProd":    h.cfg.RancherEnabledProd,
		// Vault
		"VaultConfigured": h.vault != nil,
		"VaultAddr":       h.cfg.VaultAddr,
		// Velero
		"VeleroNamespace":     h.cfg.VeleroNamespace,
		"VeleroDefaultTTL":    h.cfg.VeleroDefaultTTL,
		"VeleroTimezone":      h.cfg.VeleroTimezone,
		"VeleroS3URL":         h.cfg.VeleroS3URL,
		"VeleroBucketPreprod": h.cfg.VeleroBucketPreprod,
		"VeleroBucketProd":    h.cfg.VeleroBucketProd,
		"VeleroTTLText":       ttlToText(h.cfg.VeleroDefaultTTL),
		// General
		"LocalAuthConfigured": h.cfg.AdminPassword != "" && h.cfg.JWTSecret != "",
	})
}

// ClusterHealth returns an HTMX fragment with the cluster connection status.
func (h *Handlers) ClusterHealth(w http.ResponseWriter, r *http.Request) {
	env := r.PathValue("env")
	client := h.k8sForEnv(env)

	if client == nil {
		h.renderPartial(w, "cluster_health.html", map[string]interface{}{
			"Status":  "unconfigured",
			"Message": "Client non configuré",
		})
		return
	}

	if err := client.TestConnection(r.Context()); err != nil {
		h.renderPartial(w, "cluster_health.html", map[string]interface{}{
			"Status":  "error",
			"Message": err.Error(),
		})
		return
	}

	h.renderPartial(w, "cluster_health.html", map[string]interface{}{
		"Status":  "ok",
		"Message": "Connexion OK",
	})
}

func (h *Handlers) render(w http.ResponseWriter, name string, data interface{}) {
	if m, ok := data.(map[string]interface{}); ok {
		m["Version"] = version.Version
	}
	tmpl, ok := h.pages[name]
	if !ok {
		slog.Error("template not found", "name", name)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		slog.Error("template execute failed", "name", name, "err", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (h *Handlers) renderPartial(w http.ResponseWriter, name string, data interface{}) {
	var buf bytes.Buffer
	if err := h.partials.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("partial template execute failed", "name", name, "err", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (h *Handlers) getUser(r *http.Request) map[string]interface{} {
	user := auth.UserFromRequest(r)
	user["isAdmin"] = auth.IsAdmin(r)
	return user
}

// redirectWithFlash does an HX-Redirect and stores a flash message in a cookie
// so the next page load can display it as a toast.
func (h *Handlers) redirectWithFlash(w http.ResponseWriter, redirectURL, level, message string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "flash",
		Value:    url.QueryEscape(level + "|" + message),
		Path:     "/",
		MaxAge:   30,
		HttpOnly: false, // must be readable by JS
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("HX-Redirect", redirectURL)
}

// requireAdmin checks if the user is admin. Returns false and sends a 403 toast if not.
func (h *Handlers) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if auth.IsAdmin(r) {
		return true
	}
	w.WriteHeader(http.StatusForbidden)
	h.renderToast(w, "error", "Accès refusé : droits administrateur requis")
	return false
}

// sendNotification fires a webhook notification if one is configured.
// Errors are only logged (non-blocking).
func (h *Handlers) sendNotification(text string) {
	if h.notifier == nil {
		return
	}
	if err := h.notifier.Send(text); err != nil {
		slog.Warn("webhook notification failed", "err", err)
	}
}

// addMigration marks an app as being migrated between two vclusters (expires after 15 min).
func (h *Handlers) addMigration(env, sourceName, targetName, appName string) {
	h.migrationsMu.Lock()
	defer h.migrationsMu.Unlock()
	entry := migrationEntry{
		Source:    sourceName,
		Target:    targetName,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}
	h.migrations[fmt.Sprintf("%s:%s:%s", env, sourceName, appName)] = entry
	h.migrations[fmt.Sprintf("%s:%s:%s", env, targetName, appName)] = entry
}

// UpdateVeleroConfig updates the global Velero settings and commits BSL config to fluxprod (admin only).
func (h *Handlers) UpdateVeleroConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderToast(w, "error", "Formulaire invalide")
		return
	}

	newTTL := parseTTLText(r.FormValue("velero_ttl"))
	newS3URL := r.FormValue("velero_s3_url")
	newBucketPreprod := r.FormValue("velero_bucket_preprod")
	newBucketProd := r.FormValue("velero_bucket_prod")

	h.cfg.SetVeleroConfig(newTTL, newS3URL, newBucketPreprod, newBucketProd)

	// Refresh the generator with the new TTL
	h.generator = gitops.NewGenerator(gitops.GeneratorConfig{
		BaseDomainPreprod:   h.cfg.BaseDomainPreprod,
		BaseDomainProd:      h.cfg.BaseDomainProd,
		TLSSecretPreprod:    h.cfg.TLSSecretPreprod,
		TLSSecretProd:       h.cfg.TLSSecretProd,
		OIDCIssuer:          h.cfg.KeycloakURL + "/auth/realms/" + h.cfg.KeycloakRealm,
		GitLabSSHURL:        h.cfg.GitLabSSHURL,
		GitLabArgoCDPath:    h.cfg.GitLabArgoCDPath,
		DefaultCPU:          h.cfg.DefaultCPU,
		DefaultMemory:       h.cfg.DefaultMemory,
		DefaultStorage:      h.cfg.DefaultStorage,
		VeleroTimezone:      h.cfg.VeleroTimezone,
		VeleroDefaultTTL:    h.cfg.VeleroDefaultTTL,
		VClusterPodSecurity: h.cfg.VClusterPodSecurity,
		ArgoCDDefaultPolicy: h.cfg.ArgoCDDefaultPolicy,
	})

	// Commit BSL values.yaml to fluxprod for both envs if bucket or S3 URL is set
	if h.gitlab != nil && (h.cfg.VeleroS3URL != "" || h.cfg.VeleroBucketPreprod != "" || h.cfg.VeleroBucketProd != "") {
		var actions []gitops.CommitAction
		for _, env := range []string{"preprod", "prod"} {
			bucket := h.cfg.VeleroBucketPreprod
			if env == "prod" {
				bucket = h.cfg.VeleroBucketProd
			}
			if bucket == "" {
				continue
			}
			path := fmt.Sprintf("%s/%s/velero/values.yaml", h.cfg.FluxprodClustersPath, env)
			action := "update"
			if _, err := h.gitlab.GetFile("preprod", path); err != nil {
				action = "create"
			}
			actions = append(actions, gitops.CommitAction{
				Action:  action,
				Path:    path,
				Content: generateVeleroValuesYAML(bucket, h.cfg.VeleroS3URL),
			})
		}
		if len(actions) > 0 {
			if err := h.gitlab.Commit("preprod", "chore: update Velero BSL configuration", actions); err != nil {
				slog.Error("UpdateVeleroConfig: commit failed", "err", err)
				h.renderToast(w, "error", fmt.Sprintf("Settings sauvegardés mais erreur commit fluxprod : %v", err))
				return
			}
		}
	}

	h.renderToast(w, "success", "Configuration Velero mise à jour et commitée dans fluxprod")
}

// parseTTLText parses a short TTL string ("30j", "12h", "90m") into a Velero-compatible
// Go duration string (e.g. "720h0m0s"). Returns "" if the input is invalid or empty.
func parseTTLText(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" {
		return ""
	}
	var suffix byte
	for _, s := range []byte{'j', 'h', 'm'} {
		if text[len(text)-1] == s {
			suffix = s
			break
		}
	}
	if suffix == 0 {
		return ""
	}
	n, err := strconv.Atoi(text[:len(text)-1])
	if err != nil || n <= 0 {
		return ""
	}
	switch suffix {
	case 'j':
		return fmt.Sprintf("%dh0m0s", n*24)
	case 'h':
		return fmt.Sprintf("%dh0m0s", n)
	case 'm':
		return fmt.Sprintf("0h%dm0s", n)
	}
	return ""
}

// ttlToText converts a Velero TTL string (e.g. "720h0m0s") to short display form ("30j", "12h", "90m").
func ttlToText(ttl string) string {
	if ttl == "" {
		return ""
	}
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return ttl
	}
	total := int(d.Minutes())
	if total == 0 {
		return ""
	}
	hours := total / 60
	minutes := total % 60
	if minutes == 0 && hours > 0 {
		if hours%24 == 0 {
			return fmt.Sprintf("%dj", hours/24)
		}
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", total)
}

// generateVeleroValuesYAML generates the velero values.yaml content for a given env.
func generateVeleroValuesYAML(bucket, s3URL string) string {
	s3Line := ""
	if s3URL != "" {
		s3Line = fmt.Sprintf("\n        s3Url: \"%s\"", s3URL)
	}
	return fmt.Sprintf(`configuration:
  backupStorageLocation:
    - name: default
      provider: aws
      bucket: %s
      config:%s
        s3ForcePathStyle: "true"
        checksumAlgorithm: ""
`, bucket, s3Line)
}

// getMigrationLabel returns a non-empty label if the app is currently being migrated,
// and removes the entry if it has expired.
func (h *Handlers) getMigrationLabel(env, vcName, appName string) string {
	h.migrationsMu.Lock()
	defer h.migrationsMu.Unlock()
	key := fmt.Sprintf("%s:%s:%s", env, vcName, appName)
	entry, ok := h.migrations[key]
	if !ok {
		return ""
	}
	if time.Now().After(entry.ExpiresAt) {
		delete(h.migrations, key)
		return ""
	}
	if vcName == entry.Source {
		return "Migre vers " + entry.Target
	}
	return "Depuis " + entry.Source
}
