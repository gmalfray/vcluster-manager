package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/vault"
)

// TestRetryVaultSetup_WritesAuditLine locks D4: relaunching the Vault setup
// must leave a trace. Before the fix, this handler never called audit.Log at
// all.
//
// Uses kubernetes.NewTestStatusClient() (a fake dynamic client, no real
// cluster) rather than a bare &kubernetes.StatusClient{} — the latter has a
// nil dynamic.Interface, and the background goroutine this handler launches
// calls methods on it immediately, which panics instead of erroring. The
// fake client returns a normal "not found", so the goroutine just loops
// harmlessly in the background instead of crashing the test binary.
func TestRetryVaultSetup_WritesAuditLine(t *testing.T) {
	h := minimalHandlers()
	h.cfg = &config.Config{}
	h.vault = vault.NewClient("https://vault.example.com", "fake-token")
	h.k8sClients["preprod"] = kubernetes.NewTestStatusClient()

	w := httptest.NewRecorder()
	r := adminRequest(http.MethodPost, "/api/vclusters/demo/vault-setup-retry?env=preprod")
	r.SetPathValue("name", "demo")

	logs := captureAuditLog(func() {
		h.RetryVaultSetup(w, r)
	})

	if !strings.Contains(logs, "audit=true") || !strings.Contains(logs, "action=vault-setup-retry") {
		t.Errorf("expected an audit line for vault-setup-retry, got:\n%s", logs)
	}
	if !strings.Contains(logs, "vcluster=demo") || !strings.Contains(logs, "env=preprod") {
		t.Errorf("expected the audit line to name the vcluster and env, got:\n%s", logs)
	}
}

func TestRetryVaultSetup_ForbiddenWritesNoAuditLine(t *testing.T) {
	h := minimalHandlers()
	h.cfg = &config.Config{}
	h.vault = vault.NewClient("https://vault.example.com", "fake-token")
	h.k8sClients["preprod"] = kubernetes.NewTestStatusClient()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/vclusters/demo/vault-setup-retry?env=preprod", nil)
	r.SetPathValue("name", "demo")

	logs := captureAuditLog(func() {
		h.RetryVaultSetup(w, r)
	})

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a request without a session, got %d", w.Code)
	}
	if strings.Contains(logs, "audit=true") {
		t.Errorf("a refused request must not leave an audit line, got:\n%s", logs)
	}
}

func TestRetryVaultSetup_VaultNilWritesNoAuditLine(t *testing.T) {
	h := minimalHandlers()
	h.cfg = &config.Config{}
	// h.vault stays nil: Vault isn't configured on this deployment.

	w := httptest.NewRecorder()
	r := adminRequest(http.MethodPost, "/api/vclusters/demo/vault-setup-retry?env=preprod")
	r.SetPathValue("name", "demo")

	logs := captureAuditLog(func() {
		h.RetryVaultSetup(w, r)
	})

	if strings.Contains(logs, "audit=true") {
		t.Errorf("a no-op (Vault not configured) must not leave an audit line, got:\n%s", logs)
	}
}
