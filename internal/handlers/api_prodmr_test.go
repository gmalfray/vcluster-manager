package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
