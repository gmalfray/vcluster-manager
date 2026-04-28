package gitops

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"text/template"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

//go:embed templates
var templateFS embed.FS

// GeneratedFile represents a file to be committed.
type GeneratedFile struct {
	Path    string
	Content string
}

// GeneratorConfig holds the site-specific settings injected into YAML templates.
type GeneratorConfig struct {
	BaseDomainPreprod string // e.g. "preprod.example.com"
	BaseDomainProd    string // e.g. "example.com"
	TLSSecretPreprod  string // e.g. "wildcard-preprod-example-com-tls"
	TLSSecretProd     string // e.g. "wildcard-example-com-tls"
	OIDCIssuer        string // e.g. "https://keycloak.example.com/auth/realms/myrealm"
	GitLabSSHURL      string // e.g. "ssh://git@gitlab.example.com:22226"
	GitLabArgoCDPath  string // e.g. "ops/argocd"

	// vCluster defaults
	DefaultCPU          string // e.g. "8"
	DefaultMemory       string // e.g. "32Gi"
	DefaultStorage      string // e.g. "500Gi"
	VeleroTimezone      string // e.g. "Europe/Paris"
	VeleroDefaultTTL    string // e.g. "720h0m0s"
	VClusterPodSecurity string // e.g. "privileged"
	ArgoCDDefaultPolicy string // e.g. "role:readonly"
}

// TemplateData holds all variables available to YAML templates.
type TemplateData struct {
	Name string
	Env  string

	// Velero
	VeleroEnabled  bool
	VeleroSchedule string // precomputed cron expression
	VeleroTTL      string // e.g. "720h0m0s"

	// Quotas
	NoQuotas bool
	CPU      string
	Memory   string
	Storage  string

	// API / networking
	APIHost    string // precomputed from name+env
	K8sVersion string

	// Cert-manager
	Domain         string // precomputed from name+env
	WildcardSecret string // precomputed from name

	// ArgoCD
	ArgoCD         bool
	ArgoCDVersion  string
	ArgoCDClientID string // precomputed from name+env
	ArgoCDURL      string // precomputed from name+env (with trailing slash)
	ArgoCDHost     string // precomputed from name+env (without trailing slash)
	TargetRevision string // "preprod" or "master"
	TLSSecret      string // precomputed from env
	EnvLabel       string // "preprod" or "prod"
	PolicyLines    string // precomputed RBAC lines (4-space indented)
	OIDCIssuer     string // OIDC issuer URL for ArgoCD ConfigMap
	GitLabSSHBase  string // SSH base URL for app-manifests repos, e.g. "ssh://git@.../ops/argocd"
	DefaultPolicy  string // default ArgoCD RBAC policy

	// FluxCD
	FluxCD        bool
	FluxCDRepoURL string
	FluxCDBranch  string
	FluxCDPath    string

	// Pod security
	PodSecurity string // podSecurityStandard for the vcluster
}

// Generator creates vcluster YAML files from embedded templates.
type Generator struct {
	templates map[string]*template.Template
	cfg       GeneratorConfig
}

func NewGenerator(cfg GeneratorConfig) *Generator {
	g := &Generator{
		templates: make(map[string]*template.Template),
		cfg:       cfg,
	}
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tmpl") {
			return err
		}
		content, err := templateFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read template %s: %w", path, err)
		}
		tmpl, err := template.New(path).Parse(string(content))
		if err != nil {
			return fmt.Errorf("parse template %s: %w", path, err)
		}
		g.templates[strings.TrimPrefix(path, "templates/")] = tmpl
		return nil
	})
	if err != nil {
		slog.Error("failed to load vcluster templates", "err", err)
		os.Exit(1)
	}
	return g
}

// TODO: render() is called on a runtime path; an os.Exit on a bad template
// kills the whole server. Returning an error up the stack would be safer,
// but it's a bigger refactor (see TODO.md).
func (g *Generator) render(templatePath string, data TemplateData) string {
	tmpl, ok := g.templates[templatePath]
	if !ok {
		slog.Error("template not found", "path", templatePath)
		os.Exit(1)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		slog.Error("template render failed", "path", templatePath, "err", err)
		os.Exit(1)
	}
	return buf.String()
}

// buildData prepares the TemplateData for a given vcluster configuration.
func (g *Generator) buildData(name, env string, req *models.CreateRequest, k8sVersion string) TemplateData {
	var baseDomain string
	if env == "preprod" {
		baseDomain = g.cfg.BaseDomainPreprod
	} else {
		baseDomain = g.cfg.BaseDomainProd
	}

	apiHost := fmt.Sprintf("%s.api.%s", name, baseDomain)
	domain := fmt.Sprintf("%s.%s", name, baseDomain)
	wildcardSecret := fmt.Sprintf("wildcard-%s-tls", strings.ReplaceAll(domain, ".", "-"))

	var argocdClientID, argocdURL, argocdHost, targetRevision, tlsSecret, envLabel string
	if env == "preprod" {
		argocdClientID = fmt.Sprintf("argocd-k8s-%s-preprod", name)
		argocdHost = fmt.Sprintf("argocd.%s.%s", name, baseDomain)
		argocdURL = "https://" + argocdHost + "/"
		targetRevision = "preprod"
		tlsSecret = g.cfg.TLSSecretPreprod
		envLabel = "preprod"
	} else {
		argocdClientID = fmt.Sprintf("argocd-k8s-%s", name)
		argocdHost = fmt.Sprintf("argocd.%s.%s", name, baseDomain)
		argocdURL = "https://" + argocdHost + "/"
		targetRevision = "master"
		tlsSecret = g.cfg.TLSSecretProd
		envLabel = "prod"
	}

	var veleroSchedule string
	if req.VeleroEnabled {
		h, m := parseVeleroHour(req.VeleroHour)
		veleroSchedule = fmt.Sprintf("CRON_TZ=%s %d %d * * *", g.cfg.VeleroTimezone, m, h)
	}
	veleroTTL := req.VeleroTTL
	if veleroTTL == "" {
		veleroTTL = g.cfg.VeleroDefaultTTL
	}

	cpu := req.CPU
	if cpu == "" {
		cpu = g.cfg.DefaultCPU
	}
	mem := req.Memory
	if mem == "" {
		mem = g.cfg.DefaultMemory
	}
	storage := req.Storage
	if storage == "" {
		storage = g.cfg.DefaultStorage
	}

	var policyLines strings.Builder
	for _, group := range req.RBACGroups {
		group = strings.TrimSpace(group)
		if group != "" {
			fmt.Fprintf(&policyLines, "    g, %s, role:admin\n", group)
		}
	}

	gitlabSSHBase := g.cfg.GitLabSSHURL + "/" + g.cfg.GitLabArgoCDPath

	return TemplateData{
		Name:           name,
		Env:            env,
		APIHost:        apiHost,
		K8sVersion:     k8sVersion,
		VeleroEnabled:  req.VeleroEnabled,
		VeleroSchedule: veleroSchedule,
		VeleroTTL:      veleroTTL,
		NoQuotas:       req.NoQuotas,
		CPU:            cpu,
		Memory:         mem,
		Storage:        storage,
		Domain:         domain,
		WildcardSecret: wildcardSecret,
		ArgoCD:         req.ArgoCD,
		ArgoCDVersion:  req.ArgoCDVersion,
		ArgoCDClientID: argocdClientID,
		ArgoCDURL:      argocdURL,
		ArgoCDHost:     argocdHost,
		TargetRevision: targetRevision,
		TLSSecret:      tlsSecret,
		EnvLabel:       envLabel,
		PolicyLines:    policyLines.String(),
		OIDCIssuer:     g.cfg.OIDCIssuer,
		GitLabSSHBase:  gitlabSSHBase,
		DefaultPolicy:  g.cfg.ArgoCDDefaultPolicy,
		PodSecurity:    g.cfg.VClusterPodSecurity,
		FluxCD:         req.FluxCDEnabled,
		FluxCDRepoURL:  req.FluxCDRepoURL,
		FluxCDBranch:   req.FluxCDBranch,
		FluxCDPath:     req.FluxCDPath,
	}
}

// GenerateVCluster produces all files for a vcluster in a given env.
func (g *Generator) GenerateVCluster(req *models.CreateRequest, env string) []GeneratedFile {
	name := req.Name
	base := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
	data := g.buildData(name, env, req, "")

	files := []GeneratedFile{
		{Path: base + "/kustomization.yaml", Content: g.render("kustomization.yaml.tmpl", data)},
		{Path: base + "/values.yaml", Content: g.render("values.yaml.tmpl", data)},
		{Path: base + "/tenant_flux.yaml", Content: g.render("tenant_flux.yaml.tmpl", data)},
		{Path: base + "/tenant/kustomization.yaml", Content: g.render("tenant/kustomization.yaml.tmpl", data)},
		{Path: base + "/tenant/cert-manager_kustomization.yaml", Content: g.render("tenant/cert-manager_kustomization.yaml.tmpl", data)},
		{Path: base + "/tenant/cert-manager-config_kustomization.yaml", Content: g.render("tenant/cert-manager-config_kustomization.yaml.tmpl", data)},
		{Path: base + "/tenant/vault-webhook_kustomization.yaml", Content: g.render("tenant/vault-webhook_kustomization.yaml.tmpl", data)},
		{Path: base + "/tenant/cert-manager/kustomization.yaml", Content: g.render("tenant/cert-manager/kustomization.yaml.tmpl", data)},
		{Path: base + "/tenant/vault-webhook/kustomization.yaml", Content: g.render("tenant/vault-webhook/kustomization.yaml.tmpl", data)},
	}

	if req.ArgoCD {
		files = append(files,
			GeneratedFile{Path: base + "/tenant/argocd_kustomization.yaml", Content: g.render("tenant/argocd_kustomization.yaml.tmpl", data)},
			GeneratedFile{Path: base + "/tenant/argocd/kustomization.yaml", Content: g.render("tenant/argocd/kustomization.yaml.tmpl", data)},
			GeneratedFile{Path: base + "/tenant/argocd/argo-cd-cm.yaml", Content: g.render("tenant/argocd/argo-cd-cm.yaml.tmpl", data)},
			GeneratedFile{Path: base + "/tenant/argocd/argocd-rbac-cm.yaml", Content: g.render("tenant/argocd/argocd-rbac-cm.yaml.tmpl", data)},
			GeneratedFile{Path: base + "/tenant/navlink_kustomization.yaml", Content: g.render("tenant/navlink_kustomization.yaml.tmpl", data)},
			GeneratedFile{Path: base + "/tenant/navlink/kustomization.yaml", Content: g.render("tenant/navlink/kustomization.yaml.tmpl", data)},
		)
	}

	if req.FluxCDEnabled {
		files = append(files,
			GeneratedFile{Path: base + "/tenant/flux-bootstrap_kustomization.yaml", Content: g.render("tenant/flux-bootstrap_kustomization.yaml.tmpl", data)},
			GeneratedFile{Path: base + "/tenant/flux-bootstrap/kustomization.yaml", Content: g.render("tenant/flux-bootstrap/kustomization.yaml.tmpl", data)},
		)
	}

	return files
}

// GenerateUpdatedFluxBootstrapOverlay produces the flux-bootstrap overlay for an update.
func (g *Generator) GenerateUpdatedFluxBootstrapOverlay(name, env, repoURL, branch, path string) GeneratedFile {
	base := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
	data := TemplateData{
		Name:          name,
		FluxCDRepoURL: repoURL,
		FluxCDBranch:  branch,
		FluxCDPath:    path,
	}
	return GeneratedFile{
		Path:    base + "/tenant/flux-bootstrap/kustomization.yaml",
		Content: g.render("tenant/flux-bootstrap/kustomization.yaml.tmpl", data),
	}
}

// GenerateUpdatedValues produces just the values.yaml for an update.
func (g *Generator) GenerateUpdatedValues(name, env string, req *models.UpdateRequest) GeneratedFile {
	cr := &models.CreateRequest{
		Name:          name,
		VeleroEnabled: req.VeleroEnabled,
		VeleroHour:    req.VeleroHour,
		CPU:           req.CPU,
		Memory:        req.Memory,
		Storage:       req.Storage,
		NoQuotas:      req.NoQuotas,
		FluxCDEnabled: req.FluxCDEnabled,
		FluxCDRepoURL: req.FluxCDRepoURL,
		FluxCDBranch:  req.FluxCDBranch,
		FluxCDPath:    req.FluxCDPath,
	}
	data := g.buildData(name, env, cr, req.K8sVersion)
	base := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
	return GeneratedFile{
		Path:    base + "/values.yaml",
		Content: g.render("values.yaml.tmpl", data),
	}
}

// GenerateUpdatedArgocdOverlay produces the argocd kustomization.yaml for an update.
func (g *Generator) GenerateUpdatedArgocdOverlay(name, env, argoCDVersion string) GeneratedFile {
	cr := &models.CreateRequest{Name: name, ArgoCD: true, ArgoCDVersion: argoCDVersion}
	data := g.buildData(name, env, cr, "")
	base := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
	return GeneratedFile{
		Path:    base + "/tenant/argocd/kustomization.yaml",
		Content: g.render("tenant/argocd/kustomization.yaml.tmpl", data),
	}
}

// GenerateUpdatedRBAC produces the argocd-rbac-cm.yaml for an update.
func (g *Generator) GenerateUpdatedRBAC(name, env string, groups []string) GeneratedFile {
	cr := &models.CreateRequest{Name: name, RBACGroups: groups}
	data := g.buildData(name, env, cr, "")
	base := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
	return GeneratedFile{
		Path:    base + "/tenant/argocd/argocd-rbac-cm.yaml",
		Content: g.render("tenant/argocd/argocd-rbac-cm.yaml.tmpl", data),
	}
}

// UpdateKustomization adds or removes a vcluster entry in the cluster kustomization.yaml.
func UpdateKustomization(content, name string, add bool) string {
	entry := fmt.Sprintf("  - ./vclusters/%s", name)
	lines := strings.Split(content, "\n")

	if add {
		for _, line := range lines {
			if strings.TrimSpace(line) == strings.TrimSpace(entry) {
				return content
			}
		}
		lastVClusterIdx := -1
		for i, line := range lines {
			if strings.Contains(line, "./vclusters/") {
				lastVClusterIdx = i
			}
		}
		if lastVClusterIdx >= 0 {
			newLines := make([]string, 0, len(lines)+1)
			newLines = append(newLines, lines[:lastVClusterIdx+1]...)
			newLines = append(newLines, entry)
			newLines = append(newLines, lines[lastVClusterIdx+1:]...)
			return strings.Join(newLines, "\n")
		}
		return content + entry + "\n"
	}

	var newLines []string
	for _, line := range lines {
		if strings.TrimSpace(line) != strings.TrimSpace(entry) {
			newLines = append(newLines, line)
		}
	}
	return strings.Join(newLines, "\n")
}

func parseVeleroHour(hourStr string) (hour, minute int) {
	parts := strings.Split(hourStr, ":")
	if len(parts) == 2 {
		fmt.Sscanf(parts[0], "%d", &hour)
		fmt.Sscanf(parts[1], "%d", &minute)
	}
	return
}
