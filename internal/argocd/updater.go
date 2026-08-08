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

// UpdateResult reports what an UpdateGlobalVersion call actually did.
//
// The method used to return a bare (string, error), which conflated two very
// different outcomes: "the ArgoCD version never reached preprod" and "it did,
// but the MR to master could not be opened". Only the first means nothing
// happened. Reporting the second as a plain error told the operator that the
// update had failed while preprod had, in fact, already moved — and it invited
// a retry, which then produced a second, redundant commit.
//
// This mirrors helmcharts.UpdateResult deliberately: the two updaters do the
// same thing to the same repo, and had the same defect. Fixing one and not the
// other would leave the trap in place on the path nobody looked at.
type UpdateResult struct {
	// AlreadyApplied is true when preprod already carried the target version
	// before this call: the commit step was skipped, only the MR step ran.
	AlreadyApplied bool
	// MRURL is set once a preprod→master MR exists for this update, whether
	// this call created it or found one already open.
	MRURL string
	// MRErr is set when preprod was updated but the MR could not be created
	// or found. Kept out of the method's error return so a caller checking
	// only that return never reads "the MR failed" as "nothing happened".
	MRErr error
}

// UpdateGlobalVersion updates the ArgoCD image tag on preprod and opens a MR to
// master. Calling it twice with the same tag is safe: once preprod is at the
// target version the commit is skipped and only the MR step is retried.
func (u *Updater) UpdateGlobalVersion(ctx context.Context, tag string) (UpdateResult, error) {
	current, err := u.GetGlobalVersion(ctx, gitops.SourceBranch)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("reading current ArgoCD version: %w", err)
	}

	commitMsg := fmt.Sprintf("feat: update ArgoCD to %s", tag)

	result := UpdateResult{}
	if current == tag {
		result.AlreadyApplied = true
	} else {
		actions, err := u.buildUpdateActions(ctx, gitops.SourceBranch, tag)
		if err != nil {
			return UpdateResult{}, fmt.Errorf("building actions: %w", err)
		}
		if err := u.gitlab.Commit(ctx, gitops.SourceBranch, commitMsg, actions); err != nil {
			return UpdateResult{}, fmt.Errorf("committing to preprod: %w", err)
		}
	}

	// Past this point preprod carries the target version, whether this call
	// put it there or found it already there. An MR failure below can no
	// longer be mistaken for the version change itself having failed.
	if mr := u.GetPendingMR(); mr != nil {
		result.MRURL = mr.WebURL
		return result, nil
	}

	mrURL, err := u.gitlab.CreateMergeRequest(
		"preprod",
		"master",
		commitMsg,
		fmt.Sprintf("Mise a jour d'ArgoCD vers %s en production.", tag),
	)
	if err != nil {
		result.MRErr = fmt.Errorf("creating merge request: %w", err)
		return result, nil
	}
	result.MRURL = mrURL

	return result, nil
}

// GetPendingMR returns any open MR for ArgoCD updates.
func (u *Updater) GetPendingMR() *PendingMR {
	mrs, err := u.gitlab.ListOpenMergeRequests(gitops.DeployedBranch, gitops.SourceBranch)
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
