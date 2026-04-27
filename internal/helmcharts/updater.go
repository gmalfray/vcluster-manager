package helmcharts

import (
	"fmt"
	"strings"

	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"gopkg.in/yaml.v3"
)

// Updater manages vcluster chart updates in the platform-helm-charts repo.
type Updater struct {
	gitlab    *gitops.GitLabClient
	chartPath string // path to vcluster chart dir, e.g. "charts/vcluster"
}

// NewUpdater creates a new chart updater.
// chartPath is the directory containing Chart.yaml and values.yaml
// (e.g. "charts/vcluster").
func NewUpdater(gl *gitops.GitLabClient, chartPath string) *Updater {
	if chartPath == "" {
		chartPath = "charts/vcluster"
	}
	return &Updater{gitlab: gl, chartPath: chartPath}
}

// GetCurrentChartVersion reads the chart version from charts/vcluster/Chart.yaml on the given branch.
func (u *Updater) GetCurrentChartVersion(branch string) (string, error) {
	content, err := u.gitlab.GetFile(branch, u.chartPath+"/Chart.yaml")
	if err != nil {
		return "", fmt.Errorf("reading Chart.yaml: %w", err)
	}

	var chart struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal([]byte(content), &chart); err != nil {
		return "", fmt.Errorf("parsing Chart.yaml: %w", err)
	}
	return chart.Version, nil
}

// GetDefaultK8sVersion reads the default K8s version from charts/vcluster/values.yaml.
// Path: vcluster.controlPlane.distro.k8s.image.tag
func (u *Updater) GetDefaultK8sVersion(branch string) (string, error) {
	content, err := u.gitlab.GetFile(branch, u.chartPath+"/values.yaml")
	if err != nil {
		return "", fmt.Errorf("reading values.yaml: %w", err)
	}

	var values struct {
		VCluster struct {
			ControlPlane struct {
				Distro struct {
					K8s struct {
						Image struct {
							Tag string `yaml:"tag"`
						} `yaml:"image"`
					} `yaml:"k8s"`
				} `yaml:"distro"`
			} `yaml:"controlPlane"`
		} `yaml:"vcluster"`
	}
	if err := yaml.Unmarshal([]byte(content), &values); err != nil {
		return "", fmt.Errorf("parsing values.yaml: %w", err)
	}
	return values.VCluster.ControlPlane.Distro.K8s.Image.Tag, nil
}

// PendingMR holds info about an open merge request.
type PendingMR struct {
	Title  string
	WebURL string
}

// GetPendingChartMR returns any open MR from preprod targeting master for chart updates.
func (u *Updater) GetPendingChartMR() *PendingMR {
	mrs, err := u.gitlab.ListOpenMergeRequests("master", "preprod")
	if err != nil {
		return nil
	}
	for _, mr := range mrs {
		if strings.Contains(mr.Title, "update vcluster chart") {
			return &PendingMR{Title: mr.Title, WebURL: mr.WebURL}
		}
	}
	return nil
}

// GetPendingK8sMR returns any open MR from preprod targeting master for K8s version updates.
func (u *Updater) GetPendingK8sMR() *PendingMR {
	mrs, err := u.gitlab.ListOpenMergeRequests("master", "preprod")
	if err != nil {
		return nil
	}
	for _, mr := range mrs {
		if strings.Contains(mr.Title, "update default K8s version") {
			return &PendingMR{Title: mr.Title, WebURL: mr.WebURL}
		}
	}
	return nil
}

// UpdateChart bumps the chart version (and dependency version) in Chart.yaml.
// Commits on preprod, then creates a MR preprod → master for prod (if no MR already open).
func (u *Updater) UpdateChart(tag string) (string, error) {
	semver := trimV(tag)

	// Commit on preprod (always)
	actions, err := u.buildChartVersionActions("preprod", semver)
	if err != nil {
		return "", fmt.Errorf("building actions: %w", err)
	}

	commitMsg := fmt.Sprintf("feat: update vcluster chart to %s", tag)
	if err := u.gitlab.Commit("preprod", commitMsg, actions); err != nil {
		return "", fmt.Errorf("committing to preprod: %w", err)
	}

	// MR preprod → master (skip if one already exists)
	if mr := u.GetPendingChartMR(); mr != nil {
		return mr.WebURL, nil
	}

	mrURL, err := u.gitlab.CreateMergeRequest(
		"preprod",
		"master",
		commitMsg,
		fmt.Sprintf("Mise a jour du chart vcluster vers %s en production.", tag),
	)
	if err != nil {
		return "", fmt.Errorf("creating merge request: %w", err)
	}

	return mrURL, nil
}

// UpdateK8sVersion updates vcluster.controlPlane.distro.k8s.image.tag in values.yaml.
// Commits on preprod, then creates a MR preprod → master for prod (if no MR already open).
func (u *Updater) UpdateK8sVersion(version string) (string, error) {
	// Commit on preprod (always)
	actions, err := u.buildK8sVersionActions("preprod", version)
	if err != nil {
		return "", fmt.Errorf("building actions: %w", err)
	}

	commitMsg := fmt.Sprintf("feat: update default K8s version to %s", version)
	if err := u.gitlab.Commit("preprod", commitMsg, actions); err != nil {
		return "", fmt.Errorf("committing to preprod: %w", err)
	}

	// MR preprod → master (skip if one already exists)
	if mr := u.GetPendingK8sMR(); mr != nil {
		return mr.WebURL, nil
	}

	mrURL, err := u.gitlab.CreateMergeRequest(
		"preprod",
		"master",
		commitMsg,
		fmt.Sprintf("Mise a jour de la version K8s par defaut vers %s en production.", version),
	)
	if err != nil {
		return "", fmt.Errorf("creating merge request: %w", err)
	}

	return mrURL, nil
}

// buildChartVersionActions reads Chart.yaml from the given branch and updates
// both the chart version and the vcluster dependency version.
func (u *Updater) buildChartVersionActions(branch, semver string) ([]gitops.CommitAction, error) {
	content, err := u.gitlab.GetFile(branch, u.chartPath+"/Chart.yaml")
	if err != nil {
		return nil, fmt.Errorf("reading Chart.yaml on %s: %w", branch, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("parsing Chart.yaml: %w", err)
	}

	// Update top-level version
	setYAMLNodeValue(&doc, []string{"version"}, semver)

	// Update appVersion if present
	setYAMLNodeValue(&doc, []string{"appVersion"}, semver)

	// Update dependency version (dependencies[0].version)
	setDependencyVersion(&doc, "vcluster", semver)

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("marshaling Chart.yaml: %w", err)
	}

	return []gitops.CommitAction{{
		Action:  "update",
		Path:    u.chartPath + "/Chart.yaml",
		Content: string(out),
	}}, nil
}

// buildK8sVersionActions reads values.yaml from the given branch and updates
// vcluster.controlPlane.distro.k8s.image.tag.
func (u *Updater) buildK8sVersionActions(branch, version string) ([]gitops.CommitAction, error) {
	content, err := u.gitlab.GetFile(branch, u.chartPath+"/values.yaml")
	if err != nil {
		return nil, fmt.Errorf("reading values.yaml on %s: %w", branch, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("parsing values.yaml: %w", err)
	}

	if !setYAMLNodeValue(&doc, []string{"vcluster", "controlPlane", "distro", "k8s", "image", "tag"}, version) {
		return nil, fmt.Errorf("could not find vcluster.controlPlane.distro.k8s.image.tag in values.yaml")
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("marshaling values.yaml: %w", err)
	}

	return []gitops.CommitAction{{
		Action:  "update",
		Path:    u.chartPath + "/values.yaml",
		Content: string(out),
	}}, nil
}

// setYAMLNodeValue traverses a yaml.Node tree and sets the value at the given key path.
func setYAMLNodeValue(node *yaml.Node, path []string, value string) bool {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return setYAMLNodeValue(node.Content[0], path, value)
	}

	if node.Kind != yaml.MappingNode || len(path) == 0 {
		return false
	}

	key := path[0]
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			if len(path) == 1 {
				node.Content[i+1].Value = value
				node.Content[i+1].Tag = "!!str"
				node.Content[i+1].Kind = yaml.ScalarNode
				return true
			}
			return setYAMLNodeValue(node.Content[i+1], path[1:], value)
		}
	}
	return false
}

// setDependencyVersion finds a dependency by name in the dependencies list and updates its version.
func setDependencyVersion(node *yaml.Node, depName, version string) bool {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return setDependencyVersion(node.Content[0], depName, version)
	}
	if node.Kind != yaml.MappingNode {
		return false
	}

	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == "dependencies" && node.Content[i+1].Kind == yaml.SequenceNode {
			for _, item := range node.Content[i+1].Content {
				if item.Kind != yaml.MappingNode {
					continue
				}
				nameMatch := false
				for j := 0; j < len(item.Content)-1; j += 2 {
					if item.Content[j].Value == "name" && item.Content[j+1].Value == depName {
						nameMatch = true
						break
					}
				}
				if nameMatch {
					for j := 0; j < len(item.Content)-1; j += 2 {
						if item.Content[j].Value == "version" {
							item.Content[j+1].Value = version
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

func trimV(tag string) string {
	if len(tag) > 0 && tag[0] == 'v' {
		return tag[1:]
	}
	return tag
}
