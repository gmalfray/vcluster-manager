package helmcharts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
func (u *Updater) GetCurrentChartVersion(ctx context.Context, branch string) (string, error) {
	content, err := u.gitlab.GetFile(ctx, branch, u.chartPath+"/Chart.yaml")
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
func (u *Updater) GetDefaultK8sVersion(ctx context.Context, branch string) (string, error) {
	content, err := u.gitlab.GetFile(ctx, branch, u.chartPath+"/values.yaml")
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

// UpdateResult reports the outcome of a two-step "commit to preprod, then
// open a preprod→master MR" update. The two steps fail independently, and a
// plain (string, error) return can't say which one did: this is what let
// UpdateChart commit a version bump and then report the whole call as a
// failure when only the MR step broke (see the 2026-08-08 recette, D3 in
// docs/recette-cycle-de-vie.md). AlreadyApplied/MRErr let the caller show
// the real state instead of a blanket error, and let a retry be safe: the
// commit step is skipped once preprod already matches the target version.
type UpdateResult struct {
	// AlreadyApplied is true when preprod already had the target version
	// before this call: the commit step was skipped, only the MR step ran.
	AlreadyApplied bool
	// MRURL is set once a preprod→master MR exists for this update (created
	// now, or found already pending).
	MRURL string
	// MRErr is set when the commit to preprod succeeded but the MR could not
	// be created or found. Kept separate from the method's error return so a
	// caller checking only that return never mistakes "the MR failed" for
	// "nothing happened" — the version change already reached preprod.
	MRErr error
}

// chartDependency mirrors helm.sh/helm/v3/pkg/chart.Dependency closely
// enough to reproduce the exact Chart.lock digest Helm computes, without
// shelling out to a helm binary (this app has none, in dev or in
// production). Field names, json tags and declaration order all matter:
// Helm's digest is a sha256 of a raw JSON encoding of two dependency lists,
// so any difference in field order changes the hash. Verified against a
// real `helm dependency update` for both a single-dependency chart and a
// two-dependency chart with a condition set — see updater_test.go.
type chartDependency struct {
	Name         string        `yaml:"name" json:"name"`
	Version      string        `yaml:"version,omitempty" json:"version,omitempty"`
	Repository   string        `yaml:"repository" json:"repository"`
	Condition    string        `yaml:"condition,omitempty" json:"condition,omitempty"`
	Tags         []string      `yaml:"tags,omitempty" json:"tags,omitempty"`
	Enabled      bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	ImportValues []interface{} `yaml:"import-values,omitempty" json:"import-values,omitempty"`
	Alias        string        `yaml:"alias,omitempty" json:"alias,omitempty"`
}

// chartLock mirrors helm.sh/helm/v3/pkg/chart.Lock, the structure Chart.lock
// is deserialized into. Field order here is purely cosmetic — the digest
// covers only the dependency lists (see computeLockDigest), never the lock
// file's own bytes — but it matches what `helm dependency update` itself
// writes, so a diff against a real lock file stays readable.
type chartLock struct {
	Dependencies []chartDependency `yaml:"dependencies"`
	Digest       string            `yaml:"digest"`
	Generated    time.Time         `yaml:"generated"`
}

// computeLockDigest reproduces Helm's resolver.HashReq: a sha256 of the JSON
// encoding of [reqDeps, lockedDeps], where lockedDeps carries only
// name/repository/version — Helm drops condition/tags/enabled/import-values/
// alias when it resolves a dependency into the lock file. Getting this
// wrong produces a Chart.lock that "helm dependency build" rejects with
// "out of sync", which is worse than shipping no lock at all (a silently
// broken pin looks fine until someone tries to use it) — hence the
// byte-for-byte verification against a real Helm binary in the tests.
func computeLockDigest(deps []chartDependency) (digest string, locked []chartDependency, err error) {
	locked = make([]chartDependency, len(deps))
	for i, d := range deps {
		locked[i] = chartDependency{Name: d.Name, Repository: d.Repository, Version: d.Version}
	}

	reqPtrs := make([]*chartDependency, len(deps))
	for i := range deps {
		reqPtrs[i] = &deps[i]
	}
	lockPtrs := make([]*chartDependency, len(locked))
	for i := range locked {
		lockPtrs[i] = &locked[i]
	}

	data, err := json.Marshal([2][]*chartDependency{reqPtrs, lockPtrs})
	if err != nil {
		return "", nil, fmt.Errorf("encoding dependencies: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), locked, nil
}

// buildChartLockContent renders a Chart.lock from the (already updated)
// dependency list of Chart.yaml.
func buildChartLockContent(deps []chartDependency) (string, error) {
	digest, locked, err := computeLockDigest(deps)
	if err != nil {
		return "", fmt.Errorf("computing lock digest: %w", err)
	}
	lock := chartLock{
		Dependencies: locked,
		Digest:       digest,
		Generated:    time.Now().UTC(),
	}
	out, err := yaml.Marshal(&lock)
	if err != nil {
		return "", fmt.Errorf("marshaling Chart.lock: %w", err)
	}
	return string(out), nil
}

// GetPendingChartMR returns any open MR from preprod targeting master for chart updates.
func (u *Updater) GetPendingChartMR() *PendingMR {
	mrs, err := u.gitlab.ListOpenMergeRequests(gitops.DeployedBranch, gitops.SourceBranch)
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
	mrs, err := u.gitlab.ListOpenMergeRequests(gitops.DeployedBranch, gitops.SourceBranch)
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

// UpdateChart bumps the chart version, the appVersion and the vcluster
// dependency version in Chart.yaml, and regenerates Chart.lock in the same
// commit so the two never drift apart (Chart.lock pinning a dependency
// version that Chart.yaml no longer declares is what made "helm dependency
// build" refuse the repo and left HelmReleases running the old version
// while the UI showed the new one — see docs/recette-cycle-de-vie.md, cas 2).
// Then it creates a MR preprod → master for prod (if no MR already open).
//
// Calling this twice with the same tag is safe: if preprod is already at
// the target version, the commit step is skipped and only the MR step is
// retried. That matters because a failed MR creation used to look like a
// total failure to the caller, which invited exactly that retry — and
// retrying used to mean a second, redundant commit.
func (u *Updater) UpdateChart(ctx context.Context, tag string) (UpdateResult, error) {
	semver := trimV(tag)

	current, err := u.GetCurrentChartVersion(ctx, gitops.SourceBranch)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("reading current chart version: %w", err)
	}

	result := UpdateResult{}
	if current == semver {
		result.AlreadyApplied = true
	} else {
		actions, deps, err := u.buildChartVersionActions(ctx, gitops.SourceBranch, semver)
		if err != nil {
			return UpdateResult{}, fmt.Errorf("building actions: %w", err)
		}
		if len(deps) > 0 {
			lockAction, err := u.buildChartLockAction(ctx, gitops.SourceBranch, deps)
			if err != nil {
				return UpdateResult{}, fmt.Errorf("building Chart.lock: %w", err)
			}
			actions = append(actions, lockAction)
		}

		commitMsg := fmt.Sprintf("feat: update vcluster chart to %s", tag)
		if err := u.gitlab.Commit(ctx, gitops.SourceBranch, commitMsg, actions); err != nil {
			return UpdateResult{}, fmt.Errorf("committing to preprod: %w", err)
		}
	}

	// From here preprod already has the target version, whether this call
	// put it there or found it already there — an MR failure below can no
	// longer be mistaken for the version change itself having failed.
	if mr := u.GetPendingChartMR(); mr != nil {
		result.MRURL = mr.WebURL
		return result, nil
	}

	mrURL, err := u.gitlab.CreateMergeRequest(
		"preprod",
		"master",
		fmt.Sprintf("feat: update vcluster chart to %s", tag),
		fmt.Sprintf("Mise a jour du chart vcluster vers %s en production.", tag),
	)
	if err != nil {
		result.MRErr = fmt.Errorf("creating merge request: %w", err)
		return result, nil
	}
	result.MRURL = mrURL
	return result, nil
}

// UpdateK8sVersion updates vcluster.controlPlane.distro.k8s.image.tag in
// values.yaml. Same commit/MR split and retry-safety as UpdateChart.
func (u *Updater) UpdateK8sVersion(ctx context.Context, version string) (UpdateResult, error) {
	current, err := u.GetDefaultK8sVersion(ctx, gitops.SourceBranch)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("reading current K8s version: %w", err)
	}

	result := UpdateResult{}
	if current == version {
		result.AlreadyApplied = true
	} else {
		actions, err := u.buildK8sVersionActions(ctx, gitops.SourceBranch, version)
		if err != nil {
			return UpdateResult{}, fmt.Errorf("building actions: %w", err)
		}

		commitMsg := fmt.Sprintf("feat: update default K8s version to %s", version)
		if err := u.gitlab.Commit(ctx, gitops.SourceBranch, commitMsg, actions); err != nil {
			return UpdateResult{}, fmt.Errorf("committing to preprod: %w", err)
		}
	}

	if mr := u.GetPendingK8sMR(); mr != nil {
		result.MRURL = mr.WebURL
		return result, nil
	}

	mrURL, err := u.gitlab.CreateMergeRequest(
		"preprod",
		"master",
		fmt.Sprintf("feat: update default K8s version to %s", version),
		fmt.Sprintf("Mise a jour de la version K8s par defaut vers %s en production.", version),
	)
	if err != nil {
		result.MRErr = fmt.Errorf("creating merge request: %w", err)
		return result, nil
	}
	result.MRURL = mrURL
	return result, nil
}

// buildChartVersionActions reads Chart.yaml from the given branch and updates
// both the chart version and the vcluster dependency version. It also
// returns the resulting dependency list (post-update), so the caller can
// regenerate Chart.lock from the exact same state that gets committed —
// re-fetching Chart.yaml separately for that would risk reading it back
// before the commit lands, or a mismatched version if something else wrote
// to preprod in between.
func (u *Updater) buildChartVersionActions(ctx context.Context, branch, semver string) ([]gitops.CommitAction, []chartDependency, error) {
	content, err := u.gitlab.GetFile(ctx, branch, u.chartPath+"/Chart.yaml")
	if err != nil {
		return nil, nil, fmt.Errorf("reading Chart.yaml on %s: %w", branch, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, nil, fmt.Errorf("parsing Chart.yaml: %w", err)
	}

	// Update top-level version
	setYAMLNodeValue(&doc, []string{"version"}, semver)

	// Update appVersion if present
	setYAMLNodeValue(&doc, []string{"appVersion"}, semver)

	// Update dependency version (dependencies[0].version)
	setDependencyVersion(&doc, "vcluster", semver)

	var parsed struct {
		Dependencies []chartDependency `yaml:"dependencies"`
	}
	if err := doc.Decode(&parsed); err != nil {
		return nil, nil, fmt.Errorf("decoding updated dependencies: %w", err)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling Chart.yaml: %w", err)
	}

	return []gitops.CommitAction{{
		Action:  "update",
		Path:    u.chartPath + "/Chart.yaml",
		Content: string(out),
	}}, parsed.Dependencies, nil
}

// buildChartLockAction renders the regenerated Chart.lock and picks the
// right GitLab commit action for it. The file normally already exists
// (every vcluster chart depends on the vcluster chart), but a repo missing
// it — first update after adding a dependency, or a previous inconsistent
// state — must not turn into a GitLab "file already exists"/"file does not
// exist" error, so this checks rather than assumes "update".
func (u *Updater) buildChartLockAction(ctx context.Context, branch string, deps []chartDependency) (gitops.CommitAction, error) {
	content, err := buildChartLockContent(deps)
	if err != nil {
		return gitops.CommitAction{}, err
	}

	action := "update"
	if _, err := u.gitlab.GetFile(ctx, branch, u.chartPath+"/Chart.lock"); err != nil {
		action = "create"
	}

	return gitops.CommitAction{
		Action:  action,
		Path:    u.chartPath + "/Chart.lock",
		Content: content,
	}, nil
}

// buildK8sVersionActions reads values.yaml from the given branch and updates
// vcluster.controlPlane.distro.k8s.image.tag.
func (u *Updater) buildK8sVersionActions(ctx context.Context, branch, version string) ([]gitops.CommitAction, error) {
	content, err := u.gitlab.GetFile(ctx, branch, u.chartPath+"/values.yaml")
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
