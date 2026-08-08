package argocd

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
)

// The ArgoCD updater writes to the same repo as the chart updater, through the
// same two-step dance: commit on preprod, then open a MR to master. It carried
// the same defect — a failed MR was reported as a total failure, hiding a commit
// that had already landed — and it had no tests at all, which is why the defect
// survived the pass that fixed the chart side.
//
// These tests are the mutation guard for that fix. Each one fails if the
// corresponding decision in UpdateGlobalVersion is reverted.

const argocdKustomizationFixture = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - argocd-resources.yaml
images:
  - name: quay.io/argoproj/argocd
    newTag: v2.11.0
`

type capturedAction struct {
	Action  string `json:"action"`
	Path    string `json:"file_path"`
	Content string `json:"content"`
}

type fakeArgoRepo struct {
	mu          sync.Mutex
	files       map[string]string
	commits     []capturedAction
	mrCreated   int
	mrFail      bool
	existingMRs bool
}

func newFakeArgoRepo(files map[string]string) *fakeArgoRepo {
	return &fakeArgoRepo{files: files}
}

func (f *fakeArgoRepo) commitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commits)
}

func (f *fakeArgoRepo) createdMRs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mrCreated
}

func (f *fakeArgoRepo) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeArgoRepo) handleGetFile(w http.ResponseWriter, r *http.Request) {
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

func (f *fakeArgoRepo) handleCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CommitMessage string           `json:"commit_message"`
		Actions       []capturedAction `json:"actions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	// A real commit moves the branch forward. Mirror that, so a second call in
	// the same test sees the new state — without it the idempotency test would
	// pass for the wrong reason.
	for _, a := range body.Actions {
		f.files[a.Path] = a.Content
	}
	f.commits = append(f.commits, body.Actions...)
	f.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": "deadbeef"})
}

func (f *fakeArgoRepo) handleMergeRequests(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		if f.existingMRs {
			// source_branch matters: ListOpenMergeRequests filters on it.
			// The title must contain "update ArgoCD" for GetPendingMR to
			// recognise it as one of ours.
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"title": "feat: update ArgoCD to v2.12.0", "web_url": "https://gitlab.example.com/mr/pending", "source_branch": "preprod"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{})
	case http.MethodPost:
		if f.mrFail {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string][]string{"target_branch": {"does not exist"}},
			})
			return
		}
		f.mrCreated++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"web_url": "https://gitlab.example.com/mr/new"})
	default:
		http.NotFound(w, r)
	}
}

func newTestArgoUpdater(t *testing.T, srv *httptest.Server) *Updater {
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
	return NewUpdater(gl, "lib/tenant-template/argocd/base/kustomization.yaml")
}

const argocdKustomizationPath = "lib/tenant-template/argocd/base/kustomization.yaml"

// TestUpdateGlobalVersion_MRFailureDoesNotHideTheSuccessfulCommit is the
// central one. Before the fix the method returned ("", err) when the MR call
// failed, so a caller saw a plain error and concluded nothing had happened —
// while preprod was already on the new version. Every ArgoCD instance would
// have started rolling over on a change the operator believed had failed.
func TestUpdateGlobalVersion_MRFailureDoesNotHideTheSuccessfulCommit(t *testing.T) {
	repo := newFakeArgoRepo(map[string]string{argocdKustomizationPath: argocdKustomizationFixture})
	repo.mrFail = true

	u := newTestArgoUpdater(t, repo.server(t))
	result, err := u.UpdateGlobalVersion(context.Background(), "v2.12.0")

	// The method must NOT report a failure: the version change did land.
	if err != nil {
		t.Fatalf("expected no hard error when only the MR failed, got %v", err)
	}
	if result.MRErr == nil {
		t.Error("expected MRErr to carry the MR failure, got nil — the failure vanished entirely")
	}
	if repo.commitCount() == 0 {
		t.Fatal("expected the commit to preprod to have happened")
	}
	if !strings.Contains(repo.commits[0].Content, "v2.12.0") {
		t.Errorf("commit did not carry the new tag: %q", repo.commits[0].Content)
	}
}

// TestUpdateGlobalVersion_SkipsCommitWhenAlreadyAtTargetVersion guards the
// retry path opened by the test above. A user told "the MR failed, you may
// retry" will retry; without the idempotency check that retry writes a second,
// identical commit to preprod.
func TestUpdateGlobalVersion_SkipsCommitWhenAlreadyAtTargetVersion(t *testing.T) {
	repo := newFakeArgoRepo(map[string]string{argocdKustomizationPath: argocdKustomizationFixture})
	u := newTestArgoUpdater(t, repo.server(t))

	first, err := u.UpdateGlobalVersion(context.Background(), "v2.12.0")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if first.AlreadyApplied {
		t.Error("first call should have done the actual update, not reported it already applied")
	}
	if got := repo.commitCount(); got != 1 {
		t.Fatalf("expected exactly 1 commit after the first call, got %d", got)
	}

	second, err := u.UpdateGlobalVersion(context.Background(), "v2.12.0")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !second.AlreadyApplied {
		t.Error("second call should report AlreadyApplied")
	}
	if got := repo.commitCount(); got != 1 {
		t.Errorf("expected the second call to skip the commit, got %d commits total", got)
	}
}

// TestUpdateGlobalVersion_UsesPendingMRInsteadOfCreatingASecondOne: when a MR
// is already open for this update, reuse it. Opening a second one leaves two
// competing MRs on the same branch pair, and whichever gets merged last wins.
func TestUpdateGlobalVersion_UsesPendingMRInsteadOfCreatingASecondOne(t *testing.T) {
	repo := newFakeArgoRepo(map[string]string{argocdKustomizationPath: argocdKustomizationFixture})
	repo.existingMRs = true

	u := newTestArgoUpdater(t, repo.server(t))
	result, err := u.UpdateGlobalVersion(context.Background(), "v2.12.0")
	if err != nil {
		t.Fatalf("UpdateGlobalVersion: %v", err)
	}

	if result.MRURL != "https://gitlab.example.com/mr/pending" {
		t.Errorf("expected the pending MR to be reused, got %q", result.MRURL)
	}
	if got := repo.createdMRs(); got != 0 {
		t.Errorf("expected no new MR to be created, got %d", got)
	}
}

// TestUpdateGlobalVersion_ReportsTheMRURLOnTheNominalPath keeps the ordinary
// case honest: the previous signature returned the URL directly, and moving to
// a struct is exactly the kind of change that silently drops a field.
func TestUpdateGlobalVersion_ReportsTheMRURLOnTheNominalPath(t *testing.T) {
	repo := newFakeArgoRepo(map[string]string{argocdKustomizationPath: argocdKustomizationFixture})
	u := newTestArgoUpdater(t, repo.server(t))

	result, err := u.UpdateGlobalVersion(context.Background(), "v2.12.0")
	if err != nil {
		t.Fatalf("UpdateGlobalVersion: %v", err)
	}
	if result.MRErr != nil {
		t.Fatalf("unexpected MRErr: %v", result.MRErr)
	}
	if result.MRURL != "https://gitlab.example.com/mr/new" {
		t.Errorf("expected the new MR URL, got %q", result.MRURL)
	}
	if got := repo.createdMRs(); got != 1 {
		t.Errorf("expected exactly 1 MR created, got %d", got)
	}
}
