package gitops

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/samber/hot"
	"github.com/xanzy/go-gitlab"

	"github.com/gmalfray/vcluster-manager/internal/metrics"
)

// GitLabClientConfig holds configuration for creating a GitLabClient.
type GitLabClientConfig struct {
	URL             string
	Token           string
	ProjectID       string
	ArgoCDGroupID   string // numeric group ID (as string) for creating repos via API
	ArgoCDPath      string // namespace path for repo lookups, e.g. "ops/argocd"
	FluxDeployKeyID int
	// Fields used for app-manifests README generation:
	DomainPreprod string
	DomainProd    string
	VaultAddr     string
	GitLabSSHURL  string
	// GitOps repo structure
	ClustersPath          string // path prefix for cluster dirs, e.g. "clusters"
	VaultKVArgoCDRootApps string // Vault KV path for ArgoCD root deploy key
	VaultKVArgoCDRepo     string // Vault KV path for ArgoCD repo deploy key
}

// GitLabClient wraps the GitLab API for fluxprod operations.
type GitLabClient struct {
	client                *gitlab.Client
	projectID             string
	argocdGroupID         string
	argocdPath            string // namespace path for repo lookups, e.g. "ops/argocd"
	fluxDeployKeyID       int
	domainPreprod         string
	domainProd            string
	vaultAddr             string
	gitlabSSHURL          string
	clustersPath          string // prefix for cluster dirs, e.g. "clusters"
	vaultKVArgoCDRootApps string
	vaultKVArgoCDRepo     string
	cache                 *hot.HotCache[string, string]
}

// withRetry retries fn up to 3 times on transient errors (network, 429, 5xx).
// The ctx aborts the back-off sleep so a server shutdown does not block on
// the longest delay (~17s cumulated).
func withRetry(ctx context.Context, op string, fn func() (*gitlab.Response, error)) error {
	delays := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}
	var lastErr error
	for i, delay := range delays {
		resp, err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		// Only retry on network errors or transient HTTP status codes
		if resp != nil && resp.StatusCode != http.StatusTooManyRequests &&
			resp.StatusCode < http.StatusInternalServerError {
			metrics.GitLabAPIErrors.WithLabelValues(op).Inc()
			return err
		}
		if i < len(delays)-1 {
			slog.Warn("GitLab API transient error, retrying",
				"op", op, "attempt", i+1, "max", 3, "delay", delay, "err", err)
			metrics.GitLabAPIRetries.WithLabelValues(op).Inc()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	metrics.GitLabAPIErrors.WithLabelValues(op).Inc()
	return lastErr
}

func NewGitLabClient(cfg GitLabClientConfig) (*GitLabClient, error) {
	client, err := gitlab.NewClient(cfg.Token, gitlab.WithBaseURL(cfg.URL+"/api/v4"))
	if err != nil {
		return nil, fmt.Errorf("creating gitlab client: %w", err)
	}
	clustersPath := cfg.ClustersPath
	if clustersPath == "" {
		clustersPath = "clusters"
	}
	// W-TinyLFU is the recommended general-purpose algorithm; capacity 1024 caps
	// memory at ~5MB for typical entries (file paths joined by \n, YAML files of
	// a few KB each). The previous map+RWMutex implementation was unbounded — a
	// long-running server discovering new vclusters/files over time would grow
	// without ever purging expired entries.
	// The cache name is the project ID so multiple clients (fluxprod + helm
	// charts) export distinct hot_* metrics without colliding.
	cacheName := cfg.ProjectID
	if cacheName == "" {
		cacheName = "default"
	}
	cache := hot.NewHotCache[string, string](hot.WTinyLFU, 1024).
		WithTTL(30 * time.Second).
		WithJanitor().
		WithPrometheusMetrics(cacheName).
		Build()

	return &GitLabClient{
		client:                client,
		projectID:             cfg.ProjectID,
		argocdGroupID:         cfg.ArgoCDGroupID,
		argocdPath:            cfg.ArgoCDPath,
		fluxDeployKeyID:       cfg.FluxDeployKeyID,
		domainPreprod:         cfg.DomainPreprod,
		domainProd:            cfg.DomainProd,
		vaultAddr:             cfg.VaultAddr,
		gitlabSSHURL:          cfg.GitLabSSHURL,
		clustersPath:          clustersPath,
		vaultKVArgoCDRootApps: cfg.VaultKVArgoCDRootApps,
		vaultKVArgoCDRepo:     cfg.VaultKVArgoCDRepo,
		cache:                 cache,
	}, nil
}

// Close stops the cache janitor goroutine. Call during graceful shutdown.
func (g *GitLabClient) Close() {
	if g.cache != nil {
		g.cache.StopJanitor()
	}
}

func (g *GitLabClient) cacheGet(key string) (string, bool) {
	v, found, _ := g.cache.Get(key)
	return v, found
}

func (g *GitLabClient) cacheSet(key, data string) {
	g.cache.Set(key, data)
}

// InvalidateCache purges all cached entries.
func (g *GitLabClient) InvalidateCache() {
	g.cache.Purge()
}

// ListFiles returns file paths under a given directory in the repo.
func (g *GitLabClient) ListFiles(ctx context.Context, branch, path string) ([]string, error) {
	key := "list:" + branch + ":" + path
	if v, ok := g.cacheGet(key); ok {
		if v == "" {
			return nil, nil
		}
		return strings.Split(v, "\n"), nil
	}

	opt := &gitlab.ListTreeOptions{
		Ref:       gitlab.Ptr(branch),
		Path:      gitlab.Ptr(path),
		Recursive: gitlab.Ptr(true),
	}

	var allNodes []*gitlab.TreeNode
	for {
		var nodes []*gitlab.TreeNode
		var resp *gitlab.Response
		if err := withRetry(ctx, "list-files", func() (*gitlab.Response, error) {
			var e error
			nodes, resp, e = g.client.Repositories.ListTree(g.projectID, opt, gitlab.WithContext(ctx))
			return resp, e
		}); err != nil {
			return nil, err
		}
		allNodes = append(allNodes, nodes...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	var paths []string
	for _, n := range allNodes {
		if n.Type == "blob" {
			paths = append(paths, n.Path)
		}
	}

	g.cacheSet(key, strings.Join(paths, "\n"))
	return paths, nil
}

// GetFile reads a file from the repo.
func (g *GitLabClient) GetFile(ctx context.Context, branch, path string) (string, error) {
	key := "get:" + branch + ":" + path
	if v, ok := g.cacheGet(key); ok {
		return v, nil
	}

	var f *gitlab.File
	if err := withRetry(ctx, "get-file", func() (*gitlab.Response, error) {
		var resp *gitlab.Response
		var e error
		f, resp, e = g.client.RepositoryFiles.GetFile(g.projectID, path, &gitlab.GetFileOptions{
			Ref: gitlab.Ptr(branch),
		}, gitlab.WithContext(ctx))
		return resp, e
	}); err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(f.Content)
	if err != nil {
		return "", fmt.Errorf("decoding base64 content: %w", err)
	}
	content := string(decoded)
	g.cacheSet(key, content)
	return content, nil
}

// CommitAction represents a single file action in a commit.
type CommitAction struct {
	Action  string // "create", "update", "delete"
	Path    string
	Content string
}

// Commit creates a commit with multiple file actions.
func (g *GitLabClient) Commit(ctx context.Context, branch, message string, actions []CommitAction) error {
	var gitlabActions []*gitlab.CommitActionOptions
	for _, a := range actions {
		actionValue := gitlab.FileActionValue(a.Action)
		opt := &gitlab.CommitActionOptions{
			Action:   gitlab.FileAction(actionValue),
			FilePath: gitlab.Ptr(a.Path),
		}
		if a.Action != "delete" {
			opt.Content = gitlab.Ptr(a.Content)
		}
		gitlabActions = append(gitlabActions, opt)
	}

	err := withRetry(ctx, "commit", func() (*gitlab.Response, error) {
		_, resp, e := g.client.Commits.CreateCommit(g.projectID, &gitlab.CreateCommitOptions{
			Branch:        gitlab.Ptr(branch),
			CommitMessage: gitlab.Ptr(message),
			Actions:       gitlabActions,
		}, gitlab.WithContext(ctx))
		return resp, e
	})
	if err == nil {
		g.InvalidateCache()
	}
	return err
}

// CreateAppManifestsRepo creates a new app-manifests repo for ArgoCD.
func (g *GitLabClient) CreateAppManifestsRepo(name string) (int, error) {
	repoName := "app-manifests-" + name

	groupID, err := strconv.Atoi(g.argocdGroupID)
	if err != nil {
		return 0, fmt.Errorf("invalid argocd group ID %q: %w", g.argocdGroupID, err)
	}

	// Try to create
	proj, _, err := g.client.Projects.CreateProject(&gitlab.CreateProjectOptions{
		Name:                 gitlab.Ptr(repoName),
		Path:                 gitlab.Ptr(repoName),
		NamespaceID:          gitlab.Ptr(groupID),
		DefaultBranch:        gitlab.Ptr("master"),
		InitializeWithReadme: gitlab.Ptr(true),
		Visibility:           gitlab.Ptr(gitlab.PrivateVisibility),
		Description:          gitlab.Ptr(fmt.Sprintf("Manifestes ArgoCD pour le vcluster %s", name)),
	})
	if err != nil {
		// Project may already exist, try to find it
		projects, _, searchErr := g.client.Projects.ListProjects(&gitlab.ListProjectsOptions{
			Search: gitlab.Ptr(repoName),
		})
		if searchErr != nil {
			return 0, fmt.Errorf("create failed: %w, search failed: %w", err, searchErr)
		}
		for _, p := range projects {
			if p.Path == repoName && strings.Contains(p.PathWithNamespace, g.argocdPath) {
				return p.ID, nil
			}
		}
		return 0, fmt.Errorf("project not found after create failure: %w", err)
	}

	projectID := proj.ID

	// Configuration is best-effort: the repo exists and is recoverable manually.
	// We collect errors and return them aggregated (with the projectID), so the
	// caller can decide whether to surface a warning or fail the user-facing flow.
	var setupErrs []error

	// Set ArgoCD avatar
	if _, _, err := g.client.Projects.EditProject(projectID, &gitlab.EditProjectOptions{
		Avatar: &gitlab.ProjectAvatar{
			Filename: "argocd.png",
			Image:    bytes.NewReader(argocdAvatarPNG),
		},
	}); err != nil {
		slog.Warn("CreateAppManifestsRepo: set avatar failed", "name", name, "err", err)
		setupErrs = append(setupErrs, fmt.Errorf("set avatar: %w", err))
	}

	// Update README.md with project documentation
	readmeContent := GenerateAppManifestsREADME(name, g)
	if _, _, err := g.client.Commits.CreateCommit(projectID, &gitlab.CreateCommitOptions{
		Branch:        gitlab.Ptr("master"),
		CommitMessage: gitlab.Ptr("docs: add project README"),
		Actions: []*gitlab.CommitActionOptions{
			{
				Action:   gitlab.FileAction(gitlab.FileActionValue("update")),
				FilePath: gitlab.Ptr("README.md"),
				Content:  gitlab.Ptr(readmeContent),
			},
		},
	}); err != nil {
		slog.Warn("CreateAppManifestsRepo: commit README failed", "name", name, "err", err)
		setupErrs = append(setupErrs, fmt.Errorf("commit README: %w", err))
	}

	// Create preprod branch
	if _, _, err := g.client.Branches.CreateBranch(projectID, &gitlab.CreateBranchOptions{
		Branch: gitlab.Ptr("preprod"),
		Ref:    gitlab.Ptr("master"),
	}); err != nil {
		slog.Warn("CreateAppManifestsRepo: create preprod branch failed", "name", name, "err", err)
		setupErrs = append(setupErrs, fmt.Errorf("create preprod branch: %w", err))
	}

	// Protect branches
	for _, branch := range []string{"preprod", "master"} {
		if _, err := g.client.ProtectedBranches.UnprotectRepositoryBranches(projectID, url.PathEscape(branch)); err != nil {
			// 404 is expected when the branch isn't yet protected — log only at debug level.
			slog.Debug("CreateAppManifestsRepo: unprotect skipped (probably 404, branch not yet protected)",
				"name", name, "branch", branch, "err", err)
		}
		if _, _, err := g.client.ProtectedBranches.ProtectRepositoryBranches(projectID, &gitlab.ProtectRepositoryBranchesOptions{
			Name:             gitlab.Ptr(branch),
			PushAccessLevel:  gitlab.Ptr(gitlab.MaintainerPermissions),
			MergeAccessLevel: gitlab.Ptr(gitlab.MaintainerPermissions),
			AllowForcePush:   gitlab.Ptr(false),
		}); err != nil {
			slog.Warn("CreateAppManifestsRepo: protect branch failed",
				"name", name, "branch", branch, "err", err)
			setupErrs = append(setupErrs, fmt.Errorf("protect %s: %w", branch, err))
		}
	}

	// Enable FluxCD deploy key
	if _, _, err := g.client.DeployKeys.EnableDeployKey(projectID, g.fluxDeployKeyID); err != nil {
		slog.Warn("CreateAppManifestsRepo: enable deploy key failed",
			"name", name, "key_id", g.fluxDeployKeyID, "err", err)
		setupErrs = append(setupErrs, fmt.Errorf("enable deploy key: %w", err))
	}

	return projectID, errors.Join(setupErrs...)
}

// AppManifestsRepoExists checks if the app-manifests repo exists for a vcluster.
func (g *GitLabClient) AppManifestsRepoExists(name string) bool {
	repoName := "app-manifests-" + name
	projects, _, err := g.client.Projects.ListProjects(&gitlab.ListProjectsOptions{
		Search: gitlab.Ptr(repoName),
	})
	if err != nil {
		return false
	}
	for _, p := range projects {
		if p.Path == repoName && strings.Contains(p.PathWithNamespace, g.argocdPath) {
			return true
		}
	}
	return false
}

// DeleteProject deletes a GitLab project by path.
func (g *GitLabClient) DeleteProject(name string) error {
	repoName := "app-manifests-" + name
	projects, _, err := g.client.Projects.ListProjects(&gitlab.ListProjectsOptions{
		Search: gitlab.Ptr(repoName),
	})
	if err != nil {
		return err
	}
	for _, p := range projects {
		if p.Path == repoName && strings.Contains(p.PathWithNamespace, g.argocdPath) {
			_, err := g.client.Projects.DeleteProject(p.ID, &gitlab.DeleteProjectOptions{})
			return err
		}
	}
	return fmt.Errorf("project %s not found", repoName)
}

// CreateBranch creates a new branch from a ref.
func (g *GitLabClient) CreateBranch(branch, ref string) error {
	_, _, err := g.client.Branches.CreateBranch(g.projectID, &gitlab.CreateBranchOptions{
		Branch: gitlab.Ptr(branch),
		Ref:    gitlab.Ptr(ref),
	})
	return err
}

// CreateMergeRequest creates a MR and returns the MR URL.
func (g *GitLabClient) CreateMergeRequest(sourceBranch, targetBranch, title, description string) (string, error) {
	mr, _, err := g.client.MergeRequests.CreateMergeRequest(g.projectID, &gitlab.CreateMergeRequestOptions{
		SourceBranch:       gitlab.Ptr(sourceBranch),
		TargetBranch:       gitlab.Ptr(targetBranch),
		Title:              gitlab.Ptr(title),
		Description:        gitlab.Ptr(description),
		RemoveSourceBranch: gitlab.Ptr(true),
	})
	if err != nil {
		return "", err
	}
	return mr.WebURL, nil
}

// GetOpenPreprodMRInfo returns the URL of the open preprod→master MR (empty string if none)
// and the set of prod vcluster names that have changed files in that MR.
func (g *GitLabClient) GetOpenPreprodMRInfo() (mrURL string, changedVClusters map[string]bool, err error) {
	changedVClusters = map[string]bool{}

	mrs, _, err := g.client.MergeRequests.ListProjectMergeRequests(g.projectID, &gitlab.ListProjectMergeRequestsOptions{
		State:        gitlab.Ptr("opened"),
		SourceBranch: gitlab.Ptr("preprod"),
		TargetBranch: gitlab.Ptr("master"),
	})
	if err != nil || len(mrs) == 0 {
		return "", changedVClusters, err
	}
	mrURL = mrs[0].WebURL

	diffs, _, err := g.client.MergeRequests.ListMergeRequestDiffs(g.projectID, mrs[0].IID, &gitlab.ListMergeRequestDiffsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
	})
	if err != nil {
		return mrURL, changedVClusters, nil // best effort: MR URL is known, diffs unavailable
	}

	prefix := g.clustersPath + "/prod/vclusters/"
	for _, d := range diffs {
		for _, p := range []string{d.NewPath, d.OldPath} {
			if strings.HasPrefix(p, prefix) {
				rest := p[len(prefix):]
				if idx := strings.IndexByte(rest, '/'); idx > 0 {
					changedVClusters[rest[:idx]] = true
				}
			}
		}
	}

	return mrURL, changedVClusters, nil
}

// GetOrCreateMergeRequest returns the URL of an existing open MR from sourceBranch→targetBranch,
// or creates a new one if none exists. Does not set RemoveSourceBranch (safe for persistent branches).
func (g *GitLabClient) GetOrCreateMergeRequest(sourceBranch, targetBranch, title, description string) (string, error) {
	// Check if a MR already exists for this source→target pair
	mrs, _, err := g.client.MergeRequests.ListProjectMergeRequests(g.projectID, &gitlab.ListProjectMergeRequestsOptions{
		State:        gitlab.Ptr("opened"),
		SourceBranch: gitlab.Ptr(sourceBranch),
		TargetBranch: gitlab.Ptr(targetBranch),
	})
	if err != nil {
		return "", fmt.Errorf("listing MRs: %w", err)
	}
	if len(mrs) > 0 {
		return mrs[0].WebURL, nil
	}

	// No existing MR — create one
	mr, _, err := g.client.MergeRequests.CreateMergeRequest(g.projectID, &gitlab.CreateMergeRequestOptions{
		SourceBranch: gitlab.Ptr(sourceBranch),
		TargetBranch: gitlab.Ptr(targetBranch),
		Title:        gitlab.Ptr(title),
		Description:  gitlab.Ptr(description),
	})
	if err != nil {
		return "", err
	}
	return mr.WebURL, nil
}

// MRInfo holds basic merge request info.
type MRInfo struct {
	Title  string
	WebURL string
}

// ListOpenMergeRequests returns open MRs targeting the given branch, optionally filtered by source branch prefix.
func (g *GitLabClient) ListOpenMergeRequests(targetBranch, sourcePrefix string) ([]MRInfo, error) {
	state := "opened"
	opts := &gitlab.ListProjectMergeRequestsOptions{
		State:        gitlab.Ptr(state),
		TargetBranch: gitlab.Ptr(targetBranch),
	}

	mrs, _, err := g.client.MergeRequests.ListProjectMergeRequests(g.projectID, opts)
	if err != nil {
		return nil, err
	}

	var result []MRInfo
	for _, mr := range mrs {
		if sourcePrefix == "" || strings.HasPrefix(mr.SourceBranch, sourcePrefix) {
			result = append(result, MRInfo{
				Title:  mr.Title,
				WebURL: mr.WebURL,
			})
		}
	}
	return result, nil
}

// DeleteBranch deletes a branch.
func (g *GitLabClient) DeleteBranch(branch string) error {
	_, err := g.client.Branches.DeleteBranch(g.projectID, branch)
	return err
}
