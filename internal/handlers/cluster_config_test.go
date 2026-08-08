package handlers

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
)

// adminMultipartRequest builds an authenticated POST with a multipart form
// body — the shape UpdateClusterConfig expects (it takes an optional
// kubeconfig file upload).
func adminMultipartRequest(target string, fields map[string]string) *http.Request {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()

	r := httptest.NewRequest(http.MethodPost, target, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	payload := `{"iss":"vcluster-manager-local"}`
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
	r.AddCookie(&http.Cookie{Name: "session_token", Value: token})
	return r
}

func clusterConfigTestHandlers(t *testing.T) *Handlers {
	t.Helper()
	t.Setenv("GITLAB_TOKEN", "test-token")
	t.Setenv("DATA_DIR", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	h := minimalHandlers()
	h.cfg = cfg
	return h
}

// TestUpdateClusterConfig_WritesAuditLine locks D4: changing which cluster
// the app talks to — a kubeconfig swap — must leave a trace. Before the fix,
// this handler didn't even import the audit package.
func TestUpdateClusterConfig_WritesAuditLine(t *testing.T) {
	h := clusterConfigTestHandlers(t)
	w := httptest.NewRecorder()
	r := adminMultipartRequest("/config/preprod", map[string]string{"cluster_label": "test-cluster"})
	r.SetPathValue("env", "preprod")

	logs := captureAuditLog(func() {
		h.UpdateClusterConfig(w, r)
	})

	if !strings.Contains(logs, "audit=true") || !strings.Contains(logs, "action=update-cluster-config") {
		t.Errorf("expected an audit line for update-cluster-config, got:\n%s", logs)
	}
	if !strings.Contains(logs, "env=preprod") {
		t.Errorf("expected the audit line to name the env, got:\n%s", logs)
	}
}

func TestUpdateClusterConfig_ForbiddenWritesNoAuditLine(t *testing.T) {
	h := clusterConfigTestHandlers(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/config/preprod", nil)
	r.SetPathValue("env", "preprod")

	logs := captureAuditLog(func() {
		h.UpdateClusterConfig(w, r)
	})

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a request without a session, got %d", w.Code)
	}
	if strings.Contains(logs, "audit=true") {
		t.Errorf("a refused request must not leave an audit line, got:\n%s", logs)
	}
}
