package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
)

// fakeGitLabMRServer serves just enough of the GitLab API for
// GetOrCreateMergeRequest: an empty list (no existing MR) and a create
// response carrying a web_url.
func fakeGitLabMRServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/merge_requests") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			w.Write([]byte("[]")) // no existing MR
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"web_url": "https://gitlab.example.com/mr/1"})
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestCreateProdMR_WritesAuditLine locks D4: opening the preprod→master MR —
// the door to production — must leave a trace. Before the fix, nothing did.
func TestCreateProdMR_WritesAuditLine(t *testing.T) {
	srv := fakeGitLabMRServer(t)
	defer srv.Close()

	gl, err := gitops.NewGitLabClient(gitops.GitLabClientConfig{
		URL:       srv.URL,
		Token:     "fake-token",
		ProjectID: "1",
	})
	if err != nil {
		t.Fatalf("NewGitLabClient: %v", err)
	}
	defer gl.Close()

	h := minimalHandlers()
	h.cfg = &config.Config{}
	h.gitlab = gl

	w := httptest.NewRecorder()
	r := adminRequest(http.MethodPost, "/api/vclusters/demo/create-prod-mr")
	r.SetPathValue("name", "demo")

	logs := captureAuditLog(func() {
		h.CreateProdMR(w, r)
	})

	if !strings.Contains(logs, "audit=true") || !strings.Contains(logs, "action=create-prod-mr") {
		t.Errorf("expected an audit line for create-prod-mr, got:\n%s", logs)
	}
	if !strings.Contains(logs, "vcluster=demo") {
		t.Errorf("expected the audit line to name the vcluster, got:\n%s", logs)
	}
}

// fakeGitLabProdMRServer serves enough of the GitLab API to exercise the
// "name the other vclusters riding along" part of CreateProdMR: it tracks
// whether the MR has been created yet (GetOrCreateMergeRequest's first GET
// must see nothing, GetOpenPreprodMRInfo's GET right after must see it), and
// serves a diff naming vclusters under clusters/prod/vclusters/.
func fakeGitLabProdMRServer(t *testing.T, otherVClusters ...string) *httptest.Server {
	t.Helper()
	created := false
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/merge_requests/1/diffs"):
			var diffs []map[string]string
			for _, name := range otherVClusters {
				diffs = append(diffs, map[string]string{
					"new_path": "clusters/prod/vclusters/" + name + "/values.yaml",
					"old_path": "clusters/prod/vclusters/" + name + "/values.yaml",
				})
			}
			_ = json.NewEncoder(w).Encode(diffs)
		case strings.Contains(r.URL.Path, "/merge_requests"):
			switch r.Method {
			case http.MethodGet:
				if !created {
					w.Write([]byte("[]"))
					return
				}
				_ = json.NewEncoder(w).Encode([]map[string]interface{}{
					{"iid": 1, "web_url": "https://gitlab.example.com/mr/1", "source_branch": "preprod", "target_branch": "master"},
				})
			case http.MethodPost:
				created = true
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]string{"web_url": "https://gitlab.example.com/mr/1"})
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestCreateProdMR_NamesOtherVClustersInTheMR locks the D4 fix: this MR is
// global, not scoped to the vcluster the button was clicked from. Before the
// fix, a click on "promouvoir demo" gave no hint that another vcluster's
// changes were riding along in the same MR.
func TestCreateProdMR_NamesOtherVClustersInTheMR(t *testing.T) {
	srv := fakeGitLabProdMRServer(t, "demo", "other-vc")
	defer srv.Close()

	gl, err := gitops.NewGitLabClient(gitops.GitLabClientConfig{
		URL:       srv.URL,
		Token:     "fake-token",
		ProjectID: "1",
	})
	if err != nil {
		t.Fatalf("NewGitLabClient: %v", err)
	}
	defer gl.Close()

	h := minimalHandlers()
	h.cfg = &config.Config{}
	h.gitlab = gl

	w := httptest.NewRecorder()
	r := adminRequest(http.MethodPost, "/api/vclusters/demo/create-prod-mr")
	r.SetPathValue("name", "demo")

	h.CreateProdMR(w, r)

	msg := flashMessage(t, w)
	if !strings.Contains(msg, "other-vc") {
		t.Errorf("expected the flash message to name other-vc (another vcluster in the same MR), got %q", msg)
	}
	if strings.Contains(msg, "demo, ") || strings.HasPrefix(msg, "demo") {
		t.Errorf("the vcluster the MR was requested from must not be listed among the \"others\", got %q", msg)
	}
}

// flashMessage extracts the message part of the "flash" cookie set by
// redirectWithFlash ("level|message", URL-escaped).
func flashMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name != "flash" {
			continue
		}
		decoded, err := url.QueryUnescape(c.Value)
		if err != nil {
			t.Fatalf("decoding flash cookie: %v", err)
		}
		parts := strings.SplitN(decoded, "|", 2)
		if len(parts) == 2 {
			return parts[1]
		}
		return decoded
	}
	t.Fatalf("no flash cookie set")
	return ""
}

func TestCreateProdMR_ForbiddenWritesNoAuditLine(t *testing.T) {
	h := minimalHandlers()
	h.cfg = &config.Config{}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/vclusters/demo/create-prod-mr", nil)
	r.SetPathValue("name", "demo")

	logs := captureAuditLog(func() {
		h.CreateProdMR(w, r)
	})

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a request without a session, got %d", w.Code)
	}
	if strings.Contains(logs, "audit=true") {
		t.Errorf("a refused request must not leave an audit line, got:\n%s", logs)
	}
}
