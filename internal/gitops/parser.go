package gitops

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// fileProvider is the subset of GitLabClient used by the parser.
// Keeping it as an interface enables unit-testing with a fake implementation.
type fileProvider interface {
	GetFile(ctx context.Context, branch, path string) (string, error)
	ListFiles(ctx context.Context, branch, path string) ([]string, error)
}

// Parser reads vcluster configurations via GitLab API.
type Parser struct {
	gitlab fileProvider
	branch string // branch to read from via GitLab API
}

func NewParser() *Parser {
	return &Parser{branch: "preprod"}
}

// SetGitLabClient sets the GitLab client used to read files.
func (p *Parser) SetGitLabClient(gl *GitLabClient) {
	p.gitlab = gl
}

// readFile reads a file via GitLab API.
func (p *Parser) readFile(ctx context.Context, path string) ([]byte, error) {
	content, err := p.gitlab.GetFile(ctx, p.branch, path)
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

// listDirs lists subdirectories under a path via GitLab API.
func (p *Parser) listDirs(ctx context.Context, path string) ([]string, error) {
	files, err := p.gitlab.ListFiles(ctx, p.branch, path)
	if err != nil {
		return nil, err
	}
	// Extract unique first-level directory names from file paths
	prefix := path + "/"
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		rel := strings.TrimPrefix(f, prefix)
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) >= 2 && !seen[parts[0]] {
			seen[parts[0]] = true
			dirs = append(dirs, parts[0])
		}
	}
	return dirs, nil
}

// pathExists checks if a path exists via GitLab API.
func (p *Parser) pathExists(ctx context.Context, path string) bool {
	files, err := p.gitlab.ListFiles(ctx, p.branch, path)
	return err == nil && len(files) > 0
}

// ListVClusters discovers all vclusters for a given environment.
func (p *Parser) ListVClusters(ctx context.Context, env string) ([]models.VCluster, error) {
	vclusterPath := fmt.Sprintf("clusters/%s/vclusters", env)

	dirs, err := p.listDirs(ctx, vclusterPath)
	if err != nil {
		return nil, fmt.Errorf("reading vclusters dir: %w", err)
	}

	results := make([]*models.VCluster, len(dirs))
	g, gctx := errgroup.WithContext(ctx)
	for i, name := range dirs {
		g.Go(func() error {
			vc, err := p.parseVClusterEnv(gctx, env, name)
			if err != nil {
				// Per-vcluster parse failures are non-fatal: log and keep going
				// (a single broken values.yaml shouldn't blank the dashboard).
				// errgroup ctx cancellation is the only error worth surfacing.
				if gctx.Err() != nil {
					return gctx.Err()
				}
				slog.Warn("skipping vcluster", "env", env, "name", name, "err", err)
				return nil
			}
			results[i] = vc
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	vclusters := make([]models.VCluster, 0, len(results))
	for _, vc := range results {
		if vc != nil {
			vclusters = append(vclusters, *vc)
		}
	}
	return vclusters, nil
}

// ParseVCluster reads a single vcluster configuration.
func (p *Parser) ParseVCluster(ctx context.Context, env, name string) (*models.VCluster, error) {
	return p.parseVClusterEnv(ctx, env, name)
}

func (p *Parser) parseVClusterEnv(ctx context.Context, env, name string) (*models.VCluster, error) {
	basePath := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)

	vc := &models.VCluster{
		Name: name,
		Env:  env,
	}

	// Check if ArgoCD is enabled by looking for argocd directory
	argocdPath := basePath + "/tenant/argocd"
	if p.pathExists(ctx, argocdPath) {
		vc.ArgoCD = true
		if err := p.parseRBACGroups(ctx, basePath, vc); err != nil {
			vc.RBACGroups = []string{}
		}
		// parseArgoCDVersion is best-effort: a missing kustomization.yaml
		// just means no per-vcluster ArgoCD version override.
		_ = p.parseArgoCDVersion(ctx, basePath, vc)
	}

	// Parse values.yaml
	if err := p.parseValues(ctx, basePath, vc); err != nil {
		return nil, err
	}

	return vc, nil
}

func (p *Parser) parseValues(ctx context.Context, basePath string, vc *models.VCluster) error {
	data, err := p.readFile(ctx, basePath+"/values.yaml")
	if err != nil {
		return fmt.Errorf("reading values.yaml: %w", err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing values.yaml: %w", err)
	}

	// Parse velero
	if vb, ok := raw["veleroBackup"].(map[string]interface{}); ok {
		if enabled, ok := vb["enabled"].(bool); ok {
			vc.Velero.Enabled = enabled
		}
		if sched, ok := vb["schedule"].(string); ok {
			vc.Velero.Schedule = sched
			h, m := parseVeleroCron(sched)
			vc.Velero.Hour = h
			vc.Velero.Minute = m
		}
		if ttl, ok := vb["ttl"].(string); ok {
			vc.Velero.TTL = ttl
		}
	}

	// Parse FluxCD config
	if fluxcd, ok := raw["fluxcd"].(map[string]interface{}); ok {
		if enabled, ok := fluxcd["enabled"].(bool); ok {
			vc.FluxCD.Enabled = enabled
		}
		if url, ok := fluxcd["repoURL"].(string); ok {
			vc.FluxCD.RepoURL = url
		}
		if branch, ok := fluxcd["branch"].(string); ok {
			vc.FluxCD.Branch = branch
		}
		if path, ok := fluxcd["path"].(string); ok {
			vc.FluxCD.Path = path
		}
	}

	// Parse K8s version from vcluster.controlPlane.distro.k8s.version (if set)
	if vcObj, ok := raw["vcluster"].(map[string]interface{}); ok {
		if cp, ok := vcObj["controlPlane"].(map[string]interface{}); ok {
			if distro, ok := cp["distro"].(map[string]interface{}); ok {
				if k8s, ok := distro["k8s"].(map[string]interface{}); ok {
					if version, ok := k8s["version"].(string); ok {
						vc.K8sVersionConfig = version
					}
				}
			}
		}
	}

	// Parse quotas from nested vcluster.policies
	vc.NoQuotas = false
	if vcObj, ok := raw["vcluster"].(map[string]interface{}); ok {
		if policies, ok := vcObj["policies"].(map[string]interface{}); ok {
			if rq, ok := policies["resourceQuota"].(map[string]interface{}); ok {
				if enabled, ok := rq["enabled"].(bool); ok && !enabled {
					vc.NoQuotas = true
				}
				if quota, ok := rq["quota"].(map[string]interface{}); ok {
					vc.Quotas.CPU = fmt.Sprint(quota["requests.cpu"])
					vc.Quotas.Memory = fmt.Sprint(quota["requests.memory"])
					vc.Quotas.Storage = fmt.Sprint(quota["requests.storage"])
				}
			}
			if lr, ok := policies["limitRange"].(map[string]interface{}); ok {
				if enabled, ok := lr["enabled"].(bool); ok && !enabled {
					vc.NoQuotas = true
				}
			}
		}
	}

	return nil
}

var cronRe = regexp.MustCompile(`(\d+)\s+(\d+)\s+\*\s+\*\s+\*`)

func parseVeleroCron(schedule string) (hour, minute int) {
	matches := cronRe.FindStringSubmatch(schedule)
	if len(matches) >= 3 {
		minute, _ = strconv.Atoi(matches[1])
		hour, _ = strconv.Atoi(matches[2])
	}
	return
}

func (p *Parser) parseRBACGroups(ctx context.Context, basePath string, vc *models.VCluster) error {
	data, err := p.readFile(ctx, basePath+"/tenant/argocd/argocd-rbac-cm.yaml")
	if err != nil {
		return err
	}

	var cm struct {
		Data struct {
			PolicyCSV string `yaml:"policy.csv"`
		} `yaml:"data"`
	}
	if err := yaml.Unmarshal(data, &cm); err != nil {
		return err
	}

	for _, line := range strings.Split(cm.Data.PolicyCSV, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "g,") {
			parts := strings.Split(line, ",")
			if len(parts) >= 2 {
				group := strings.TrimSpace(parts[1])
				if group != "" {
					vc.RBACGroups = append(vc.RBACGroups, group)
				}
			}
		}
	}

	return nil
}

func (p *Parser) parseArgoCDVersion(ctx context.Context, basePath string, vc *models.VCluster) error {
	data, err := p.readFile(ctx, basePath+"/tenant/argocd/kustomization.yaml")
	if err != nil {
		return err
	}

	var kust struct {
		Images []struct {
			Name   string `yaml:"name"`
			NewTag string `yaml:"newTag"`
		} `yaml:"images"`
	}
	if err := yaml.Unmarshal(data, &kust); err != nil {
		return err
	}
	for _, img := range kust.Images {
		if img.Name == "quay.io/argoproj/argocd" {
			vc.ArgoCDVersion = img.NewTag
			break
		}
	}
	return nil
}

// ListVClusterNamesOnBranch returns vcluster names found on a specific branch via GitLab API.
// Returns nil if no GitLab client is configured.
func (p *Parser) ListVClusterNamesOnBranch(ctx context.Context, branch, env string) []string {
	if p.gitlab == nil {
		return nil
	}
	path := fmt.Sprintf("clusters/%s/vclusters", env)
	files, err := p.gitlab.ListFiles(ctx, branch, path)
	if err != nil {
		return nil
	}
	prefix := path + "/"
	seen := map[string]bool{}
	var names []string
	for _, f := range files {
		rel := strings.TrimPrefix(f, prefix)
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) >= 2 && !seen[parts[0]] {
			seen[parts[0]] = true
			names = append(names, parts[0])
		}
	}
	return names
}

// Exists checks if a vcluster exists for the given env.
func (p *Parser) Exists(ctx context.Context, env, name string) bool {
	path := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
	return p.pathExists(ctx, path)
}

// UsedVeleroSlots returns all velero times currently in use.
func (p *Parser) UsedVeleroSlots(ctx context.Context, env string) []string {
	vclusters, err := p.ListVClusters(ctx, env)
	if err != nil {
		return nil
	}
	var slots []string
	for _, vc := range vclusters {
		if vc.Velero.Enabled {
			slots = append(slots, fmt.Sprintf("%02d:%02d", vc.Velero.Hour, vc.Velero.Minute))
		}
	}
	return slots
}
