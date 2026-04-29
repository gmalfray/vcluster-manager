package gitops

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go"
	"gopkg.in/yaml.v3"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// findAppManifestsProjectID searches the GitLab project ID for app-manifests-{vcName}.
func (g *GitLabClient) findAppManifestsProjectID(vcName string) (int64, error) {
	repoName := "app-manifests-" + vcName
	projects, _, err := g.client.Projects.ListProjects(&gitlab.ListProjectsOptions{
		Search: gitlab.Ptr(repoName),
	})
	if err != nil {
		return 0, err
	}
	for _, p := range projects {
		if p.Path == repoName && strings.Contains(p.PathWithNamespace, "ops/argocd") {
			return p.ID, nil
		}
	}
	return 0, fmt.Errorf("project %s not found", repoName)
}

// ListAppManifestFiles lists all YAML files in the app-manifests repo for a vcluster.
func (g *GitLabClient) ListAppManifestFiles(vcName, branch string) ([]string, error) {
	projectID, err := g.findAppManifestsProjectID(vcName)
	if err != nil {
		return nil, err
	}

	opt := &gitlab.ListTreeOptions{
		Ref:       gitlab.Ptr(branch),
		Recursive: gitlab.Ptr(true),
	}

	var allNodes []*gitlab.TreeNode
	for {
		nodes, resp, err := g.client.Repositories.ListTree(projectID, opt)
		if err != nil {
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
		if n.Type != "blob" {
			continue
		}
		lower := strings.ToLower(n.Path)
		if strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") {
			paths = append(paths, n.Path)
		}
	}
	return paths, nil
}

// GetAppManifestFile reads a file from the app-manifests repo (base64-decoded content).
func (g *GitLabClient) GetAppManifestFile(vcName, branch, path string) (string, error) {
	projectID, err := g.findAppManifestsProjectID(vcName)
	if err != nil {
		return "", err
	}

	f, _, err := g.client.RepositoryFiles.GetFile(projectID, path, &gitlab.GetFileOptions{
		Ref: gitlab.Ptr(branch),
	})
	if err != nil {
		return "", err
	}

	decoded, err := base64.StdEncoding.DecodeString(f.Content)
	if err != nil {
		return "", fmt.Errorf("decoding file %s: %w", path, err)
	}
	return string(decoded), nil
}

// CommitToAppManifests commits changes to an app-manifests repo.
func (g *GitLabClient) CommitToAppManifests(vcName, branch, message string, actions []CommitAction) error {
	projectID, err := g.findAppManifestsProjectID(vcName)
	if err != nil {
		return err
	}

	var gitlabActions []*gitlab.CommitActionOptions
	for _, a := range actions {
		actionValue := gitlab.FileActionValue(a.Action)
		opt := &gitlab.CommitActionOptions{
			Action:   &actionValue,
			FilePath: gitlab.Ptr(a.Path),
		}
		if a.Action != "delete" {
			opt.Content = gitlab.Ptr(a.Content)
		}
		gitlabActions = append(gitlabActions, opt)
	}

	_, _, err = g.client.Commits.CreateCommit(projectID, &gitlab.CreateCommitOptions{
		Branch:        gitlab.Ptr(branch),
		CommitMessage: gitlab.Ptr(message),
		Actions:       gitlabActions,
	})
	return err
}

// argoAppYAML is used for parsing ArgoCD Application manifests.
type argoAppYAML struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Source struct {
			RepoURL        string `yaml:"repoURL"`
			Path           string `yaml:"path"`
			TargetRevision string `yaml:"targetRevision"`
		} `yaml:"source"`
		Destination struct {
			Namespace string `yaml:"namespace"`
		} `yaml:"destination"`
		Project string `yaml:"project"`
	} `yaml:"spec"`
}

// ParseArgoApps parses YAML content and returns ArgoCD Application objects found.
// Supports multi-document YAML (separated by ---).
// Filters: kind=Application && apiVersion contains "argoproj.io".
func ParseArgoApps(filePath, content string) []models.ArgoApp {
	var apps []models.ArgoApp

	docs := bytes.Split([]byte(content), []byte("\n---"))
	for _, doc := range docs {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}

		var app argoAppYAML
		if err := yaml.Unmarshal(doc, &app); err != nil {
			continue
		}

		if app.Kind != "Application" || !strings.Contains(app.APIVersion, "argoproj.io") {
			continue
		}

		apps = append(apps, models.ArgoApp{
			Name:          app.Metadata.Name,
			Namespace:     app.Spec.Destination.Namespace,
			FilePath:      filePath,
			SourceRepoURL: app.Spec.Source.RepoURL,
			SourcePath:    app.Spec.Source.Path,
			SourceBranch:  app.Spec.Source.TargetRevision,
			Project:       app.Spec.Project,
		})
	}
	return apps
}
