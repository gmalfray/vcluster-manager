package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// fakeGitLab is a minimal stand-in for the GitLab REST API: just enough of
// the repository tree, file and merge-request endpoints for the gitops
// package's client to talk to during a test. Populate it with addTree/addFile
// before starting the server; it is read-only once ServeHTTP runs.
type fakeGitLab struct {
	trees map[string][]string // "ref|path" -> blob paths
	files map[string]string   // "ref|path" -> raw content
	mrs   []fakeMR
}

type fakeMR struct {
	iid    int
	webURL string
}

func newFakeGitLab() *fakeGitLab {
	return &fakeGitLab{trees: map[string][]string{}, files: map[string]string{}}
}

func (f *fakeGitLab) addTree(ref, path string, blobPaths ...string) {
	f.trees[ref+"|"+path] = blobPaths
}

func (f *fakeGitLab) addFile(ref, path, content string) {
	f.files[ref+"|"+path] = content
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case strings.HasSuffix(r.URL.Path, "/merge_requests"):
		var out []map[string]interface{}
		for _, mr := range f.mrs {
			out = append(out, map[string]interface{}{"iid": mr.iid, "web_url": mr.webURL})
		}
		_ = json.NewEncoder(w).Encode(out)

	case strings.Contains(r.URL.Path, "/merge_requests/") && strings.HasSuffix(r.URL.Path, "/diffs"):
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{})

	case strings.Contains(r.URL.Path, "/repository/tree"):
		ref := r.URL.Query().Get("ref")
		path := r.URL.Query().Get("path")
		var out []map[string]interface{}
		for _, p := range f.trees[ref+"|"+path] {
			out = append(out, map[string]interface{}{"id": "x", "name": p, "type": "blob", "path": p, "mode": "100644"})
		}
		_ = json.NewEncoder(w).Encode(out)

	case strings.Contains(r.URL.Path, "/repository/files/"):
		ref := r.URL.Query().Get("ref")
		idx := strings.Index(r.URL.Path, "/repository/files/")
		path := r.URL.Path[idx+len("/repository/files/"):]
		content, ok := f.files[ref+"|"+path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "404 File Not Found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"file_name": path,
			"file_path": path,
			"size":      len(content),
			"encoding":  "base64",
			"content":   base64.StdEncoding.EncodeToString([]byte(content)),
			"ref":       ref,
		})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// newTestConfig builds a real *config.Config backed by a temp-dir file
// backend, so ListDeleting/ListCleaning/AddDeleting/AddCleaning work exactly
// as in production instead of panicking on a nil backend.
func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("GITLAB_TOKEN", "test-token")
	t.Setenv("DATA_DIR", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// newDashboardTestService wires a Service against a fake GitLab server: the
// parser reads vcluster config through it (branch "preprod", as in
// production — even for the prod env's config) and the service's own gitlab
// client fetches the open preprod→master MR through it too.
func newDashboardTestService(t *testing.T, fg *fakeGitLab, mu *sync.RWMutex, cfg *config.Config) *Service {
	t.Helper()
	srv := httptest.NewServer(fg)
	t.Cleanup(srv.Close)

	gl, err := gitops.NewGitLabClient(gitops.GitLabClientConfig{URL: srv.URL, Token: "test-token", ProjectID: "1"})
	if err != nil {
		t.Fatalf("gitops.NewGitLabClient: %v", err)
	}
	t.Cleanup(gl.Close)

	parser := gitops.NewParser()
	parser.SetGitLabClient(gl)

	return New(Deps{
		Cfg:          cfg,
		Parser:       parser,
		GitLab:       gl,
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: mu,
	})
}

func TestGetDashboard_Nominal(t *testing.T) {
	fg := newFakeGitLab()

	// preprod: one vcluster, no ArgoCD, Velero backup enabled.
	fg.addTree("preprod", "clusters/preprod/vclusters", "clusters/preprod/vclusters/demo1/values.yaml")
	fg.addFile("preprod", "clusters/preprod/vclusters/demo1/values.yaml", "veleroBackup:\n  enabled: true\n  schedule: \"0 3 * * *\"\n")

	// prod: demo2 (ArgoCD, not yet on master -> pending) and demo3 (no ArgoCD, already on master).
	fg.addTree("preprod", "clusters/prod/vclusters",
		"clusters/prod/vclusters/demo2/values.yaml",
		"clusters/prod/vclusters/demo3/values.yaml",
	)
	fg.addFile("preprod", "clusters/prod/vclusters/demo2/values.yaml", "veleroBackup:\n  enabled: false\n")
	fg.addFile("preprod", "clusters/prod/vclusters/demo3/values.yaml", "veleroBackup:\n  enabled: true\n")
	fg.addTree("preprod", "clusters/prod/vclusters/demo2/tenant/argocd", "clusters/prod/vclusters/demo2/tenant/argocd/kustomization.yaml")

	// Only demo3 has landed on master so far.
	fg.addTree("master", "clusters/prod/vclusters", "clusters/prod/vclusters/demo3/values.yaml")

	// Open preprod->master MR, picked up for the pending prod item.
	fg.mrs = []fakeMR{{iid: 42, webURL: "https://gitlab.example/mr/42"}}

	cfg := newTestConfig(t)
	cfg.AddDeleting("demo4", "prod", "https://gitlab.example/mr/deleting-4")
	cfg.AddCleaning("demo5", "preprod", false, false, false, false)

	var mu sync.RWMutex
	s := newDashboardTestService(t, fg, &mu, cfg)

	data, err := s.GetDashboard(context.Background())
	if err != nil {
		t.Fatalf("GetDashboard: %v", err)
	}

	if len(data.Groups) != 2 {
		t.Fatalf("expected 2 groups (preprod, prod), got %d: %+v", len(data.Groups), data.Groups)
	}
	preprod, prod := data.Groups[0], data.Groups[1]
	if preprod.Env != "preprod" || prod.Env != "prod" {
		t.Fatalf("expected groups in order [preprod, prod], got [%s, %s]", preprod.Env, prod.Env)
	}

	if len(preprod.Items) != 2 {
		t.Fatalf("expected 2 preprod items (demo1 + synthetic demo5), got %d: %+v", len(preprod.Items), preprod.Items)
	}
	if len(prod.Items) != 3 {
		t.Fatalf("expected 3 prod items (demo2 + demo3 + synthetic demo4), got %d: %+v", len(prod.Items), prod.Items)
	}

	byName := func(items []models.DashboardItem, name string) *models.DashboardItem {
		for i := range items {
			if items[i].VCluster.Name == name {
				return &items[i]
			}
		}
		t.Fatalf("item %q not found", name)
		return nil
	}

	demo1 := byName(preprod.Items, "demo1")
	if demo1.VCluster.ArgoCD || !demo1.VCluster.Velero.Enabled {
		t.Errorf("demo1: expected ArgoCD=false Velero.Enabled=true, got %+v", demo1.VCluster)
	}

	demo5 := byName(preprod.Items, "demo5")
	if !demo5.RancherCleaning {
		t.Errorf("demo5: expected RancherCleaning=true, got %+v", demo5)
	}

	demo2 := byName(prod.Items, "demo2")
	if !demo2.VCluster.ArgoCD {
		t.Errorf("demo2: expected ArgoCD=true")
	}
	if !demo2.PendingMR || demo2.PendingMRURL != "https://gitlab.example/mr/42" {
		t.Errorf("demo2: expected PendingMR=true with the open MR URL, got %+v", demo2)
	}

	demo3 := byName(prod.Items, "demo3")
	if demo3.PendingMR {
		t.Errorf("demo3: expected PendingMR=false (already on master), got %+v", demo3)
	}
	if !demo3.VCluster.Velero.Enabled {
		t.Errorf("demo3: expected Velero.Enabled=true")
	}

	demo4 := byName(prod.Items, "demo4")
	if !demo4.Deleting || demo4.DeletingMR != "https://gitlab.example/mr/deleting-4" {
		t.Errorf("demo4: expected Deleting=true with the deleting MR URL, got %+v", demo4)
	}

	// Summary cards.
	if data.SummaryTotalPreprod != 2 || data.SummaryTotalProd != 3 || data.SummaryTotal != 5 {
		t.Errorf("unexpected totals: preprod=%d prod=%d total=%d", data.SummaryTotalPreprod, data.SummaryTotalProd, data.SummaryTotal)
	}
	if data.SummaryArgoCDCount != 1 || data.SummaryNoArgoCDCount != 4 {
		t.Errorf("unexpected ArgoCD counts: argocd=%d no-argocd=%d", data.SummaryArgoCDCount, data.SummaryNoArgoCDCount)
	}
	if data.SummaryBackupCount != 2 || data.SummaryNoBackupCount != 3 {
		t.Errorf("unexpected backup counts: backup=%d no-backup=%d", data.SummaryBackupCount, data.SummaryNoBackupCount)
	}
	if data.SummaryPendingCount != 1 {
		t.Errorf("expected SummaryPendingCount=1, got %d", data.SummaryPendingCount)
	}

	// Optional integrations were left unconfigured: their flags/data must stay off.
	if data.HelmUpdaterEnabled || data.ArgoCDUpdaterEnabled {
		t.Errorf("expected optional updaters disabled when their client is nil")
	}
	if data.LatestRelease != nil || data.LatestArgoCDRelease != nil {
		t.Errorf("expected no release banner when the GitHub client is nil")
	}
}

func TestGetDashboard_VClusterListingFailsGracefully(t *testing.T) {
	// No trees registered on the fake server -> the GitLab API returns 404 for
	// both envs. GetDashboard must not fail: it logs and renders an empty
	// (but valid) dashboard, same as the pre-extraction handler.
	fg := newFakeGitLab()
	cfg := newTestConfig(t)
	var mu sync.RWMutex
	s := newDashboardTestService(t, fg, &mu, cfg)

	data, err := s.GetDashboard(context.Background())
	if err != nil {
		t.Fatalf("expected nil error even when vcluster listing fails, got %v", err)
	}
	if len(data.Groups) != 0 {
		t.Errorf("expected no groups, got %+v", data.Groups)
	}
	if data.SummaryTotal != 0 {
		t.Errorf("expected SummaryTotal=0, got %d", data.SummaryTotal)
	}
}

func TestGetDashboard_NoGitLabClient_NoPendingMRURL(t *testing.T) {
	// A prod vcluster missing from master is still marked pending even when
	// no GitLab client is configured to fetch the MR URL: PendingMRURL just
	// stays empty (mirrors the "if s.gitlab != nil" guard in the original handler).
	fg := newFakeGitLab()
	fg.addTree("preprod", "clusters/prod/vclusters", "clusters/prod/vclusters/demo2/values.yaml")
	fg.addFile("preprod", "clusters/prod/vclusters/demo2/values.yaml", "veleroBackup:\n  enabled: false\n")

	cfg := newTestConfig(t)

	srv := httptest.NewServer(fg)
	t.Cleanup(srv.Close)
	gl, err := gitops.NewGitLabClient(gitops.GitLabClientConfig{URL: srv.URL, Token: "test-token", ProjectID: "1"})
	if err != nil {
		t.Fatalf("gitops.NewGitLabClient: %v", err)
	}
	t.Cleanup(gl.Close)
	parser := gitops.NewParser()
	parser.SetGitLabClient(gl)

	var mu sync.RWMutex
	s := New(Deps{Cfg: cfg, Parser: parser, K8sClients: map[string]*kubernetes.StatusClient{}, K8sClientsMu: &mu}) // no GitLab dep

	data, err := s.GetDashboard(context.Background())
	if err != nil {
		t.Fatalf("GetDashboard: %v", err)
	}
	if len(data.Groups) != 1 || data.Groups[0].Env != "prod" {
		t.Fatalf("expected a single prod group, got %+v", data.Groups)
	}
	item := data.Groups[0].Items[0]
	if !item.PendingMR {
		t.Errorf("expected PendingMR=true (not on master)")
	}
	if item.PendingMRURL != "" {
		t.Errorf("expected empty PendingMRURL with no GitLab client, got %q", item.PendingMRURL)
	}
}
