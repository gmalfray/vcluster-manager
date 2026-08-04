package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// --- RBAC ------------------------------------------------------------------

func TestUpdateSettings_ForbiddenForNonAdmin(t *testing.T) {
	s := newTestService()
	_, err := s.UpdateSettings(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, "demo", "preprod", UpdateSettingsInput{})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

// --- Field validation: anti-injection --------------------------------------
//
// UpdateSettings' field checks feed straight into a text/template committed to
// fluxprod (internal/gitops/generator.go), which — unlike html/template —
// doesn't escape anything. These payloads are exactly what that template
// would let through unescaped: a YAML key injected via newline, a string
// broken out of its quotes, and a shell command appended to the flux
// bootstrap command line.

func TestUpdateSettings_RejectsInvalidName(t *testing.T) {
	s := newTestService()
	_, err := s.UpdateSettings(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "../etc/passwd", "preprod", UpdateSettingsInput{})
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
}

func TestUpdateSettings_RejectsYAMLInjectionInCPU(t *testing.T) {
	s := newTestService()
	_, err := s.UpdateSettings(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod",
		UpdateSettingsInput{CPU: "8\n  evil: true"})
	if err == nil || !strings.Contains(err.Error(), "cpu :") {
		t.Fatalf("expected a cpu validation error, got %v", err)
	}
}

func TestUpdateSettings_RejectsMaliciousK8sVersion(t *testing.T) {
	s := newTestService()
	_, err := s.UpdateSettings(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod",
		UpdateSettingsInput{K8sVersion: "1.28.4\nnewTag: evil"})
	if err == nil || !strings.Contains(err.Error(), "k8s_version :") {
		t.Fatalf("expected a k8s_version validation error, got %v", err)
	}
}

func TestUpdateSettings_RejectsQuoteBreakInArgoCDVersion(t *testing.T) {
	s := newTestService()
	_, err := s.UpdateSettings(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod",
		UpdateSettingsInput{ArgoCDVersion: `v2.9.0"`})
	if err == nil || !strings.Contains(err.Error(), "argocd_version :") {
		t.Fatalf("expected an argocd_version validation error, got %v", err)
	}
}

func TestUpdateSettings_RejectsShellInjectionInFluxBranch(t *testing.T) {
	s := newTestService()
	_, err := s.UpdateSettings(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod",
		UpdateSettingsInput{FluxCDBranch: "main; rm -rf /"})
	if err == nil || !strings.Contains(err.Error(), "fluxcd_branch :") {
		t.Fatalf("expected a fluxcd_branch validation error, got %v", err)
	}
}

func TestUpdateSettings_RejectsShellInjectionInFluxRepoURL(t *testing.T) {
	s := newTestService()
	_, err := s.UpdateSettings(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod",
		UpdateSettingsInput{FluxCDRepoURL: "ssh://git@host/repo.git`whoami`"})
	if err == nil || !strings.Contains(err.Error(), "fluxcd_repo_url :") {
		t.Fatalf("expected a fluxcd_repo_url validation error, got %v", err)
	}
}

func TestUpdateSettings_RejectsBadVeleroHour(t *testing.T) {
	s := newTestService()
	_, err := s.UpdateSettings(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod",
		UpdateSettingsInput{VeleroHour: "25:99"})
	if err == nil || !strings.Contains(err.Error(), "velero_hour :") {
		t.Fatalf("expected a velero_hour validation error, got %v", err)
	}
}

// --- ArgoCD toggle: FluxCD carried over (non-regression) -------------------
//
// settingsFakeGitLab and settingsTestServer below are a self-contained
// stand-in for the GitLab REST API — deliberately not shared with
// dashboard_test.go's fakeGitLab so this file stays independent of the
// dashboard domain.

// settingsFakeGitLab serves just enough of the GitLab API for ParseVCluster
// (tree + file reads) and Commit (captured for inspection).
type settingsFakeGitLab struct {
	trees map[string][]string // "ref|path" -> blob paths
	files map[string]string   // "ref|path" -> raw content

	mu             sync.Mutex
	committedFiles map[string]string // path -> content, from the last Commit call
}

func newSettingsFakeGitLab() *settingsFakeGitLab {
	return &settingsFakeGitLab{trees: map[string][]string{}, files: map[string]string{}}
}

func (f *settingsFakeGitLab) addTree(ref, path string, blobPaths ...string) {
	f.trees[ref+"|"+path] = blobPaths
}

func (f *settingsFakeGitLab) addFile(ref, path, content string) {
	f.files[ref+"|"+path] = content
}

func (f *settingsFakeGitLab) contentOf(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.committedFiles[path]
	return c, ok
}

func (f *settingsFakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/commits"):
		var body struct {
			Actions []struct {
				Action  string `json:"action"`
				Path    string `json:"file_path"`
				Content string `json:"content"`
			} `json:"actions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		committed := map[string]string{}
		for _, a := range body.Actions {
			if a.Action != "delete" {
				committed[a.Path] = a.Content
			}
		}
		f.mu.Lock()
		f.committedFiles = committed
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "deadbeef"})

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

// newSettingsTestService wires a Service against a fake GitLab server: the
// parser reads vcluster config through it, and the service's own gitlab
// client commits through it too.
func newSettingsTestService(t *testing.T, fg *settingsFakeGitLab) *Service {
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

	generator := gitops.NewGenerator(gitops.GeneratorConfig{})

	var mu sync.RWMutex
	return New(Deps{
		Cfg:          &config.Config{},
		Parser:       parser,
		Generator:    generator,
		GitLab:       gl,
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})
}

// TestUpdateSettings_ArgoCDToggle_PreservesFluxCDBootstrap is the regression
// test for the ArgoCD/FluxCD bug: toggling ArgoCD on a vcluster that has
// FluxCD bootstrapped used to regenerate every file of that vcluster from
// scratch without carrying the FluxCD repo URL/branch/path over, so the
// regenerated flux-bootstrap kustomization ended up with an empty
// --url=... — silently breaking the bootstrap instead of leaving it alone.
func TestUpdateSettings_ArgoCDToggle_PreservesFluxCDBootstrap(t *testing.T) {
	fg := newSettingsFakeGitLab()

	// demo: ArgoCD enabled, FluxCD bootstrapped with a real repo URL. The
	// settings form submission (in) below carries no FluxCD fields at all —
	// exactly what the UI sends when only the ArgoCD toggle is touched.
	fg.addTree("preprod", "clusters/preprod/vclusters/demo/tenant/argocd",
		"clusters/preprod/vclusters/demo/tenant/argocd/kustomization.yaml")
	fg.addFile("preprod", "clusters/preprod/vclusters/demo/values.yaml", `
fluxcd:
  enabled: true
  repoURL: "ssh://git@gitlab.example.com/ops/fluxprod.git"
  branch: "main"
  path: "clusters/prod/flux-system"
`)

	s := newSettingsTestService(t, fg)

	in := UpdateSettingsInput{
		RBACGroups:   []string{"admin"},
		ArgoCDToggle: "off", // flips ArgoCD: currently enabled -> disabled
		// FluxCDRepoURL/Branch/Path left empty: the form didn't touch them.
	}

	res, err := s.UpdateSettings(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, "demo", "preprod", in)
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if res.FlashMessage != "Configuration ArgoCD modifiée" {
		t.Fatalf("unexpected flash message: %q", res.FlashMessage)
	}

	content, ok := fg.contentOf("clusters/preprod/vclusters/demo/tenant/flux-bootstrap/kustomization.yaml")
	if !ok {
		t.Fatalf("expected a flux-bootstrap kustomization to be committed, got files: %v", fg.committedFiles)
	}
	if !strings.Contains(content, "--url=ssh://git@gitlab.example.com/ops/fluxprod.git") {
		t.Errorf("flux-bootstrap kustomization lost its repo URL after the ArgoCD toggle: %s", content)
	}
	if !strings.Contains(content, "--branch=main") {
		t.Errorf("flux-bootstrap kustomization lost its branch after the ArgoCD toggle: %s", content)
	}
}

// TestFirstNonEmpty is the pure-function complement to the regression test
// above: it's the exact mechanism the ArgoCD-toggle fix relies on.
func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Errorf("firstNonEmpty(a, b) = %q, want a", got)
	}
	if got := firstNonEmpty("", "b"); got != "b" {
		t.Errorf(`firstNonEmpty("", b) = %q, want b`, got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf(`firstNonEmpty("", "") = %q, want ""`, got)
	}
}
