package helmcharts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"gopkg.in/yaml.v3"
)

// --- Golden digest tests -----------------------------------------------
//
// These values were not invented: they come from running a real
// `helm dependency update` (helm v3.20.1) against a throwaway chart and
// reading the digest it wrote to Chart.lock. If computeLockDigest ever
// drifts from Helm's own resolver.HashReq, these are the tests that catch
// it — a hand-rolled digest that merely "looks right" is exactly the
// failure mode the D1 fix has to avoid (a Chart.lock that helm rejects with
// "out of sync" is worse than no lock at all).

func TestComputeLockDigest_MatchesRealHelm_SingleDependency(t *testing.T) {
	deps := []chartDependency{
		{Name: "vcluster", Version: "0.36.1", Repository: "https://charts.loft.sh"},
	}
	digest, locked, err := computeLockDigest(deps)
	if err != nil {
		t.Fatalf("computeLockDigest: %v", err)
	}
	const want = "sha256:9b5de45250678ae93ff2635604f59a995b8a1010cacbed500ca3f8c5a369e147"
	if digest != want {
		t.Fatalf("digest = %q, want %q (from a real `helm dependency update`)", digest, want)
	}
	if len(locked) != 1 || locked[0].Name != "vcluster" || locked[0].Version != "0.36.1" || locked[0].Repository != "https://charts.loft.sh" {
		t.Fatalf("unexpected locked dependency: %+v", locked)
	}
}

func TestComputeLockDigest_MatchesRealHelm_TwoDependenciesWithCondition(t *testing.T) {
	deps := []chartDependency{
		{Name: "vcluster", Version: "0.36.1", Repository: "https://charts.loft.sh"},
		{Name: "common", Version: "2.0.0", Repository: "https://charts.bitnami.com/bitnami", Condition: "common.enabled"},
	}
	digest, locked, err := computeLockDigest(deps)
	if err != nil {
		t.Fatalf("computeLockDigest: %v", err)
	}
	const want = "sha256:4da67791431ae5264c59258bea807aea755cdaded7d31e981193b77986d2872d"
	if digest != want {
		t.Fatalf("digest = %q, want %q (from a real `helm dependency update`)", digest, want)
	}
	// Helm drops condition/tags/enabled/import-values/alias when it resolves
	// a dependency into the lock file — only name/repository/version survive.
	if locked[1].Condition != "" {
		t.Fatalf("locked dependency must not carry Condition, got %q", locked[1].Condition)
	}
}

func TestComputeLockDigest_DifferentVersionsProduceDifferentDigests(t *testing.T) {
	// The whole point of D1: bumping the dependency version must move the
	// digest, or the lock silently keeps pinning the old chart.
	d1, _, err := computeLockDigest([]chartDependency{{Name: "vcluster", Version: "0.34.7", Repository: "https://charts.loft.sh"}})
	if err != nil {
		t.Fatalf("computeLockDigest: %v", err)
	}
	d2, _, err := computeLockDigest([]chartDependency{{Name: "vcluster", Version: "0.36.1", Repository: "https://charts.loft.sh"}})
	if err != nil {
		t.Fatalf("computeLockDigest: %v", err)
	}
	if d1 == d2 {
		t.Fatalf("digests for different dependency versions must differ, both were %q", d1)
	}
}

func TestBuildChartLockContent_ParsesBackWithNoExtraFields(t *testing.T) {
	content, err := buildChartLockContent([]chartDependency{
		{Name: "vcluster", Version: "0.36.1", Repository: "https://charts.loft.sh"},
	})
	if err != nil {
		t.Fatalf("buildChartLockContent: %v", err)
	}

	var parsed struct {
		Dependencies []map[string]interface{} `yaml:"dependencies"`
		Digest       string                   `yaml:"digest"`
		Generated    string                   `yaml:"generated"`
	}
	if err := yaml.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("Chart.lock content does not parse as YAML: %v\n%s", err, content)
	}
	if parsed.Digest == "" || !strings.HasPrefix(parsed.Digest, "sha256:") {
		t.Fatalf("expected a sha256 digest, got %q", parsed.Digest)
	}
	if parsed.Generated == "" {
		t.Fatalf("expected a generated timestamp")
	}
	if len(parsed.Dependencies) != 1 {
		t.Fatalf("expected 1 dependency, got %d", len(parsed.Dependencies))
	}
	dep := parsed.Dependencies[0]
	for _, extra := range []string{"condition", "tags", "enabled", "import-values", "alias"} {
		if _, present := dep[extra]; present {
			t.Errorf("Chart.lock dependency must not carry %q (Helm drops it when resolving), got %+v", extra, dep)
		}
	}
}

// --- UpdateChart / UpdateK8sVersion against a fake GitLab -----------------

// fakeChartRepo serves just enough of the GitLab API to exercise
// Updater.UpdateChart and Updater.UpdateK8sVersion: file reads, a commit
// endpoint that records every commit it receives, and a merge-request
// endpoint whose outcome (fail/succeed) is configurable per test.
type fakeChartRepo struct {
	mu sync.Mutex

	// files maps a path suffix ("charts/vcluster/Chart.yaml", ...) to its
	// content. A missing entry means "404 — file does not exist on this
	// branch", which the client surfaces as an error.
	files map[string]string

	commits     []capturedCommit
	mrFail      bool // when true, POST merge_requests responds 400
	existingMRs int  // when > 0, GET merge_requests returns that many "open" MRs
}

type capturedCommit struct {
	message string
	actions []struct {
		Action  string `json:"action"`
		Path    string `json:"file_path"`
		Content string `json:"content"`
	}
}

func newFakeChartRepo(files map[string]string) *fakeChartRepo {
	return &fakeChartRepo{files: files}
}

func (f *fakeChartRepo) commitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commits)
}

func (f *fakeChartRepo) lastCommit() capturedCommit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commits[len(f.commits)-1]
}

func (f *fakeChartRepo) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repository/files/"):
			f.handleGetFile(w, r)
		case strings.Contains(r.URL.Path, "/repository/commits"):
			f.handleCommit(w, r)
		case strings.Contains(r.URL.Path, "/merge_requests"):
			f.handleMergeRequests(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func (f *fakeChartRepo) handleGetFile(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for suffix, content := range f.files {
		if strings.HasSuffix(r.URL.Path, suffix) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content": base64.StdEncoding.EncodeToString([]byte(content)),
			})
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "404 File Not Found"})
}

func (f *fakeChartRepo) handleCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CommitMessage string `json:"commit_message"`
		Actions       []struct {
			Action  string `json:"action"`
			Path    string `json:"file_path"`
			Content string `json:"content"`
		} `json:"actions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	// A real commit would move the branch content forward; mirror that so a
	// subsequent GetFile in the same test sees the new state (needed for the
	// "already applied" idempotency test, which calls UpdateChart twice).
	for _, a := range body.Actions {
		f.files[a.Path] = a.Content
	}
	f.commits = append(f.commits, capturedCommit{message: body.CommitMessage, actions: body.Actions})
	f.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": "deadbeef"})
}

func (f *fakeChartRepo) handleMergeRequests(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		if f.existingMRs > 0 {
			// source_branch must be set: ListOpenMergeRequests filters
			// client-side on the source branch prefix ("preprod").
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"title": "feat: update vcluster chart to 0.36.1", "web_url": "https://gitlab.example.com/mr/pending", "source_branch": "preprod"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{})
	case http.MethodPost:
		if f.mrFail {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message": map[string][]string{"target_branch": {"does not exist"}},
			})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"web_url": "https://gitlab.example.com/mr/new"})
	default:
		http.NotFound(w, r)
	}
}

func newTestUpdater(t *testing.T, srv *httptest.Server) *Updater {
	t.Helper()
	gl, err := gitops.NewGitLabClient(gitops.GitLabClientConfig{
		URL:       srv.URL,
		Token:     "fake-token",
		ProjectID: "1",
	})
	if err != nil {
		t.Fatalf("NewGitLabClient: %v", err)
	}
	t.Cleanup(gl.Close)
	return NewUpdater(gl, "charts/vcluster")
}

const chartYAMLFixture = `apiVersion: v2
name: vcluster
version: 1.0.0
appVersion: "1.0.0"
dependencies:
  - name: vcluster
    version: 0.34.7
    repository: "https://charts.loft.sh"
`

const chartLockFixture = `dependencies:
- name: vcluster
  repository: https://charts.loft.sh
  version: 0.34.7
digest: sha256:oldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldold
generated: "2026-01-01T00:00:00Z"
`

// TestUpdateChart_RegeneratesChartLockInSameCommit locks D1: bumping the
// chart must not leave Chart.lock behind. It checks both that Chart.lock is
// part of the same commit as Chart.yaml (never a second, separate write —
// a partial failure between the two is exactly the drift the recette found)
// and that its digest matches what a real Helm would compute for the new
// version.
func TestUpdateChart_RegeneratesChartLockInSameCommit(t *testing.T) {
	repo := newFakeChartRepo(map[string]string{
		"charts/vcluster/Chart.yaml": chartYAMLFixture,
		"charts/vcluster/Chart.lock": chartLockFixture,
	})
	srv := repo.server(t)
	defer srv.Close()

	u := newTestUpdater(t, srv)
	result, err := u.UpdateChart(context.Background(), "v0.36.1")
	if err != nil {
		t.Fatalf("UpdateChart: %v", err)
	}
	if result.MRErr != nil {
		t.Fatalf("unexpected MR error: %v", result.MRErr)
	}
	if result.MRURL == "" {
		t.Fatalf("expected an MR URL")
	}

	if repo.commitCount() != 1 {
		t.Fatalf("expected exactly 1 commit, got %d", repo.commitCount())
	}
	commit := repo.lastCommit()

	var lockAction, chartAction *struct {
		Action  string
		Path    string
		Content string
	}
	for i := range commit.actions {
		a := commit.actions[i]
		switch a.Path {
		case "charts/vcluster/Chart.lock":
			lockAction = &struct{ Action, Path, Content string }{a.Action, a.Path, a.Content}
		case "charts/vcluster/Chart.yaml":
			chartAction = &struct{ Action, Path, Content string }{a.Action, a.Path, a.Content}
		}
	}
	if chartAction == nil {
		t.Fatalf("commit did not touch Chart.yaml: %+v", commit.actions)
	}
	if lockAction == nil {
		t.Fatalf("commit did not touch Chart.lock — this is D1, the chart version bumped without regenerating the lock")
	}
	if lockAction.Action != "update" {
		t.Errorf("Chart.lock already existed, expected action=update, got %q", lockAction.Action)
	}

	wantDigest, _, err := computeLockDigest([]chartDependency{
		{Name: "vcluster", Version: "0.36.1", Repository: "https://charts.loft.sh"},
	})
	if err != nil {
		t.Fatalf("computeLockDigest: %v", err)
	}
	if !strings.Contains(lockAction.Content, wantDigest) {
		t.Errorf("Chart.lock content does not carry the expected digest %q:\n%s", wantDigest, lockAction.Content)
	}
	if strings.Contains(lockAction.Content, "0.34.7") {
		t.Errorf("Chart.lock still pins the old version 0.34.7:\n%s", lockAction.Content)
	}
}

// TestUpdateChart_CreatesChartLockWhenMissing covers a repo that never had
// a Chart.lock (or lost it) — the commit action must be "create", not
// "update", or GitLab rejects it.
func TestUpdateChart_CreatesChartLockWhenMissing(t *testing.T) {
	repo := newFakeChartRepo(map[string]string{
		"charts/vcluster/Chart.yaml": chartYAMLFixture,
		// no Chart.lock entry: GetFile 404s for it.
	})
	srv := repo.server(t)
	defer srv.Close()

	u := newTestUpdater(t, srv)
	if _, err := u.UpdateChart(context.Background(), "v0.36.1"); err != nil {
		t.Fatalf("UpdateChart: %v", err)
	}

	commit := repo.lastCommit()
	for _, a := range commit.actions {
		if a.Path == "charts/vcluster/Chart.lock" {
			if a.Action != "create" {
				t.Errorf("expected action=create for a Chart.lock that didn't exist, got %q", a.Action)
			}
			return
		}
	}
	t.Fatalf("commit did not touch Chart.lock: %+v", commit.actions)
}

// TestUpdateChart_SkipsCommitWhenAlreadyAtTargetVersion is the retry-safety
// half of the D2 fix: calling UpdateChart again with the same tag (e.g.
// because the first call's MR failed) must not create a second, redundant
// commit.
func TestUpdateChart_SkipsCommitWhenAlreadyAtTargetVersion(t *testing.T) {
	// GetCurrentChartVersion reads the top-level `version` field, so that's
	// what must already match the target tag for the commit to be skipped.
	alreadyAppliedChartYAML := strings.NewReplacer(
		"version: 1.0.0", "version: 0.36.1",
		"0.34.7", "0.36.1",
	).Replace(chartYAMLFixture)
	repo := newFakeChartRepo(map[string]string{
		"charts/vcluster/Chart.yaml": alreadyAppliedChartYAML,
		"charts/vcluster/Chart.lock": chartLockFixture,
	})
	srv := repo.server(t)
	defer srv.Close()

	u := newTestUpdater(t, srv)
	result, err := u.UpdateChart(context.Background(), "v0.36.1")
	if err != nil {
		t.Fatalf("UpdateChart: %v", err)
	}
	if !result.AlreadyApplied {
		t.Errorf("expected AlreadyApplied=true when preprod is already at the target version")
	}
	if repo.commitCount() != 0 {
		t.Fatalf("expected no commit when preprod already matches the target version, got %d", repo.commitCount())
	}
	if result.MRURL == "" {
		t.Errorf("expected the MR step to still run and return a URL")
	}
}

// TestUpdateChart_MRFailureDoesNotHideTheSuccessfulCommit is D2 itself: a
// failed MR must not make the caller think the version change never
// happened, and the version change must not be lost either.
func TestUpdateChart_MRFailureDoesNotHideTheSuccessfulCommit(t *testing.T) {
	repo := newFakeChartRepo(map[string]string{
		"charts/vcluster/Chart.yaml": chartYAMLFixture,
		"charts/vcluster/Chart.lock": chartLockFixture,
	})
	repo.mrFail = true
	srv := repo.server(t)
	defer srv.Close()

	u := newTestUpdater(t, srv)
	result, err := u.UpdateChart(context.Background(), "v0.36.1")
	if err != nil {
		t.Fatalf("UpdateChart must not return a bare error when the commit succeeded, got: %v", err)
	}
	if result.MRErr == nil {
		t.Fatalf("expected MRErr to be set when MR creation fails")
	}
	if repo.commitCount() != 1 {
		t.Fatalf("expected the commit to have gone through despite the MR failure, got %d commits", repo.commitCount())
	}
	commit := repo.lastCommit()
	found := false
	for _, a := range commit.actions {
		if a.Path == "charts/vcluster/Chart.yaml" && strings.Contains(a.Content, "0.36.1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the committed Chart.yaml to carry the new version despite the MR failure")
	}
}

// TestUpdateChart_UsesPendingMRInsteadOfCreatingASecondOne checks the
// existing dedup behaviour still holds once the retry-safety logic was
// added around it.
func TestUpdateChart_UsesPendingMRInsteadOfCreatingASecondOne(t *testing.T) {
	repo := newFakeChartRepo(map[string]string{
		"charts/vcluster/Chart.yaml": chartYAMLFixture,
		"charts/vcluster/Chart.lock": chartLockFixture,
	})
	repo.existingMRs = 1
	srv := repo.server(t)
	defer srv.Close()

	u := newTestUpdater(t, srv)
	result, err := u.UpdateChart(context.Background(), "v0.36.1")
	if err != nil {
		t.Fatalf("UpdateChart: %v", err)
	}
	if result.MRURL != "https://gitlab.example.com/mr/pending" {
		t.Errorf("expected the pending MR to be reused, got %q", result.MRURL)
	}
}

const valuesYAMLFixture = `vcluster:
  controlPlane:
    distro:
      k8s:
        image:
          tag: "v1.31.0"
`

// TestUpdateK8sVersion_SkipsCommitWhenAlreadyAtTargetVersion mirrors the
// chart-side retry-safety fix for the K8s version updater.
func TestUpdateK8sVersion_SkipsCommitWhenAlreadyAtTargetVersion(t *testing.T) {
	repo := newFakeChartRepo(map[string]string{
		"charts/vcluster/values.yaml": valuesYAMLFixture,
	})
	srv := repo.server(t)
	defer srv.Close()

	u := newTestUpdater(t, srv)
	result, err := u.UpdateK8sVersion(context.Background(), "v1.31.0")
	if err != nil {
		t.Fatalf("UpdateK8sVersion: %v", err)
	}
	if !result.AlreadyApplied {
		t.Errorf("expected AlreadyApplied=true")
	}
	if repo.commitCount() != 0 {
		t.Fatalf("expected no commit, got %d", repo.commitCount())
	}
}

func TestUpdateK8sVersion_MRFailureDoesNotHideTheSuccessfulCommit(t *testing.T) {
	repo := newFakeChartRepo(map[string]string{
		"charts/vcluster/values.yaml": valuesYAMLFixture,
	})
	repo.mrFail = true
	srv := repo.server(t)
	defer srv.Close()

	u := newTestUpdater(t, srv)
	result, err := u.UpdateK8sVersion(context.Background(), "v1.32.0")
	if err != nil {
		t.Fatalf("UpdateK8sVersion must not return a bare error when the commit succeeded, got: %v", err)
	}
	if result.MRErr == nil {
		t.Fatalf("expected MRErr to be set")
	}
	if repo.commitCount() != 1 {
		t.Fatalf("expected the commit to have gone through despite the MR failure, got %d", repo.commitCount())
	}
}
