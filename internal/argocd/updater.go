package argocd

import (
	"context"
	"fmt"
	"strings"

	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"gopkg.in/yaml.v3"
)

// Updater manages ArgoCD version updates in the fluxprod repo.
type Updater struct {
	gitlab            *gitops.GitLabClient
	kustomizationPath string
}

// NewUpdater creates a new ArgoCD updater using the fluxprod GitLab client.
// kustomizationPath is the path to the base ArgoCD kustomization.yaml in the repo
// (e.g. "lib/tenant-template/argocd/base/kustomization.yaml").
func NewUpdater(gl *gitops.GitLabClient, kustomizationPath string) *Updater {
	if kustomizationPath == "" {
		kustomizationPath = "lib/tenant-template/argocd/base/kustomization.yaml"
	}
	return &Updater{gitlab: gl, kustomizationPath: kustomizationPath}
}

const argocdImageName = "quay.io/argoproj/argocd"

// PendingMR holds info about an open merge request.
type PendingMR struct {
	Title  string
	WebURL string
}

// GetGlobalVersion reads the ArgoCD image tag from the base kustomization.
func (u *Updater) GetGlobalVersion(ctx context.Context, branch string) (string, error) {
	content, err := u.gitlab.GetFile(ctx, branch, u.kustomizationPath)
	if err != nil {
		return "", fmt.Errorf("reading kustomization.yaml: %w", err)
	}
	return extractImageTag(content, argocdImageName)
}

// UpdateGlobalVersion updates the ArgoCD image tag on preprod and creates a MR to master.
func (u *Updater) UpdateGlobalVersion(ctx context.Context, tag string) (string, error) {
	actions, err := u.buildUpdateActions(ctx, "preprod", tag)
	if err != nil {
		return "", fmt.Errorf("building actions: %w", err)
	}

	commitMsg := fmt.Sprintf("feat: update ArgoCD to %s", tag)
	if err := u.gitlab.Commit(ctx, "preprod", commitMsg, actions); err != nil {
		return "", fmt.Errorf("committing to preprod: %w", err)
	}

	if mr := u.GetPendingMR(); mr != nil {
		return mr.WebURL, nil
	}

	mrURL, err := u.gitlab.CreateMergeRequest(
		"preprod",
		"master",
		commitMsg,
		fmt.Sprintf("Mise a jour d'ArgoCD vers %s en production.", tag),
	)
	if err != nil {
		return "", fmt.Errorf("creating merge request: %w", err)
	}

	return mrURL, nil
}

// GetPendingMR returns any open MR for ArgoCD updates.
func (u *Updater) GetPendingMR() *PendingMR {
	mrs, err := u.gitlab.ListOpenMergeRequests("master", "preprod")
	if err != nil {
		return nil
	}
	for _, mr := range mrs {
		if strings.Contains(mr.Title, "update ArgoCD") {
			return &PendingMR{Title: mr.Title, WebURL: mr.WebURL}
		}
	}
	return nil
}

func (u *Updater) buildUpdateActions(ctx context.Context, branch, tag string) ([]gitops.CommitAction, error) {
	content, err := u.gitlab.GetFile(ctx, branch, u.kustomizationPath)
	if err != nil {
		return nil, fmt.Errorf("reading kustomization.yaml on %s: %w", branch, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("parsing kustomization.yaml: %w", err)
	}

	if !setImageTag(&doc, argocdImageName, tag) {
		return nil, fmt.Errorf("could not find image %s in kustomization.yaml", argocdImageName)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("marshaling kustomization.yaml: %w", err)
	}

	return []gitops.CommitAction{{
		Action:  "update",
		Path:    u.kustomizationPath,
		Content: string(out),
	}}, nil
}

// extractImageTag finds the newTag for a given image name in a kustomization.yaml.
func extractImageTag(content, imageName string) (string, error) {
	var doc struct {
		Images []struct {
			Name   string `yaml:"name"`
			NewTag string `yaml:"newTag"`
		} `yaml:"images"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return "", fmt.Errorf("parsing YAML: %w", err)
	}
	for _, img := range doc.Images {
		if img.Name == imageName {
			return img.NewTag, nil
		}
	}
	return "", nil
}

// setImageTag modifies the newTag for a named image in the yaml.Node tree.
func setImageTag(node *yaml.Node, imageName, tag string) bool {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return setImageTag(node.Content[0], imageName, tag)
	}
	if node.Kind != yaml.MappingNode {
		return false
	}

	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == "images" && node.Content[i+1].Kind == yaml.SequenceNode {
			for _, item := range node.Content[i+1].Content {
				if item.Kind != yaml.MappingNode {
					continue
				}
				nameMatch := false
				for j := 0; j < len(item.Content)-1; j += 2 {
					if item.Content[j].Value == "name" && item.Content[j+1].Value == imageName {
						nameMatch = true
						break
					}
				}
				if nameMatch {
					for j := 0; j < len(item.Content)-1; j += 2 {
						if item.Content[j].Value == "newTag" {
							item.Content[j+1].Value = tag
							item.Content[j+1].Tag = "!!str"
							return true
						}
					}
				}
			}
		}
	}
	return false
}
