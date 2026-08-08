package handlers

import (
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
)

// sessionCookie encodes claims into a JWT-shaped session_token cookie for
// tests. The signature isn't checked by auth.UserFromRequest/IsAdmin, only
// its shape and the payload.
func sessionCookie(claims map[string]interface{}) *http.Cookie {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadJSON, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	return &http.Cookie{Name: "session_token", Value: header + "." + payload + ".fakesig"}
}

// minimalHandlers builds a Handlers with only the fields needed for a given test.
// All optional fields are zero/nil.
func minimalHandlers() *Handlers {
	// Minimal partials template so renderToast/renderPartial don't panic.
	partials := template.Must(template.New("toast.html").Parse(
		`{{define "toast.html"}}{{.Level}}:{{.Message}}{{end}}`,
	))
	return &Handlers{
		partials:    partials,
		migrations:  make(map[string]migrationEntry),
		vaultStates: make(map[string]*vaultSetupState),
		k8sClients:  make(map[string]*kubernetes.StatusClient),
	}
}

// --- addMigration / getMigrationLabel ---

func TestAddMigration_LabelSource(t *testing.T) {
	h := minimalHandlers()
	h.addMigration("preprod", "source-vc", "target-vc", "myapp")

	label := h.getMigrationLabel("preprod", "source-vc", "myapp")
	if !strings.Contains(label, "target-vc") {
		t.Errorf("source label should mention target, got %q", label)
	}
}

func TestAddMigration_LabelTarget(t *testing.T) {
	h := minimalHandlers()
	h.addMigration("preprod", "source-vc", "target-vc", "myapp")

	label := h.getMigrationLabel("preprod", "target-vc", "myapp")
	if !strings.Contains(label, "source-vc") {
		t.Errorf("target label should mention source, got %q", label)
	}
}

func TestAddMigration_UnknownApp(t *testing.T) {
	h := minimalHandlers()
	h.addMigration("preprod", "source-vc", "target-vc", "myapp")

	label := h.getMigrationLabel("preprod", "source-vc", "other-app")
	if label != "" {
		t.Errorf("expected empty label for unknown app, got %q", label)
	}
}

func TestAddMigration_Expiry(t *testing.T) {
	h := minimalHandlers()
	h.migrationsMu.Lock()
	h.migrations["preprod:myvc:myapp"] = migrationEntry{
		Source:    "myvc",
		Target:    "other",
		ExpiresAt: time.Now().Add(-1 * time.Second), // already expired
	}
	h.migrationsMu.Unlock()

	label := h.getMigrationLabel("preprod", "myvc", "myapp")
	if label != "" {
		t.Errorf("expected empty label for expired migration, got %q", label)
	}
	// Entry should be cleaned up
	h.migrationsMu.Lock()
	_, exists := h.migrations["preprod:myvc:myapp"]
	h.migrationsMu.Unlock()
	if exists {
		t.Error("expired migration entry should be removed")
	}
}

func TestAddMigration_DifferentEnv(t *testing.T) {
	h := minimalHandlers()
	h.addMigration("preprod", "source-vc", "target-vc", "myapp")

	// Same names but different env should return empty
	label := h.getMigrationLabel("prod", "source-vc", "myapp")
	if label != "" {
		t.Errorf("expected empty label for wrong env, got %q", label)
	}
}

// --- setVaultState / getVaultState ---

func TestVaultState_SetAndGet(t *testing.T) {
	h := minimalHandlers()
	h.setVaultState("preprod", "myvc", "done", "")

	vs := h.getVaultState("preprod", "myvc")
	if vs == nil {
		t.Fatal("expected vault state, got nil")
	}
	if vs.Status != "done" {
		t.Errorf("Status = %q, want done", vs.Status)
	}
	if vs.Message != "" {
		t.Errorf("Message = %q, want empty", vs.Message)
	}
}

func TestVaultState_Error(t *testing.T) {
	h := minimalHandlers()
	h.setVaultState("preprod", "myvc", "error", "vault unreachable")

	vs := h.getVaultState("preprod", "myvc")
	if vs == nil {
		t.Fatal("expected vault state, got nil")
	}
	if vs.Status != "error" {
		t.Errorf("Status = %q, want error", vs.Status)
	}
	if vs.Message != "vault unreachable" {
		t.Errorf("Message = %q, want 'vault unreachable'", vs.Message)
	}
}

func TestVaultState_Unknown(t *testing.T) {
	h := minimalHandlers()
	vs := h.getVaultState("preprod", "nonexistent")
	if vs != nil {
		t.Errorf("expected nil for unknown vcluster, got %+v", vs)
	}
}

func TestVaultState_Overwrite(t *testing.T) {
	h := minimalHandlers()
	h.setVaultState("preprod", "myvc", "waiting", "")
	h.setVaultState("preprod", "myvc", "done", "")

	vs := h.getVaultState("preprod", "myvc")
	if vs.Status != "done" {
		t.Errorf("Status = %q, want done after overwrite", vs.Status)
	}
}

func TestVaultState_Concurrent(t *testing.T) {
	h := minimalHandlers()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.setVaultState("preprod", "myvc", "done", "")
			h.getVaultState("preprod", "myvc")
		}()
	}
	wg.Wait()
}

// --- k8sForEnv ---

func TestK8sForEnv_ReturnsNilWhenEmpty(t *testing.T) {
	h := minimalHandlers()
	if h.k8sForEnv("preprod") != nil {
		t.Error("expected nil when no clients configured")
	}
}

// TestK8sForEnv_NoFallbackToAnotherEnv replaces TestK8sForEnv_FallbackToAny,
// which locked in the very behaviour that was the bug: with only a prod client
// configured, a request for preprod used to be answered with the prod client.
// Nothing logged it, and map iteration order made it non-deterministic which
// client you got when several were present.
//
// The stake is not cosmetic: cleanup and deletion resolve their client this
// way, so the fallback could run a deletion's cleanup against the wrong
// cluster. A single-cluster install is unaffected — cmd/server/main.go
// registers its one client under both keys.
func TestK8sForEnv_NoFallbackToAnotherEnv(t *testing.T) {
	h := minimalHandlers()
	h.k8sClients["prod"] = &kubernetes.StatusClient{}

	if h.k8sForEnv("prod") == nil {
		t.Error("expected the prod client on a direct hit")
	}
	if got := h.k8sForEnv("preprod"); got != nil {
		t.Error("preprod has no client configured: expected nil, got the client of another environment")
	}
}

// TestCleanupClient_NilStaysNilThroughTheInterface guards a trap that a plain
// `if k8s != nil` cannot catch. A nil *StatusClient stored in an interface
// yields a non-nil interface value, so the guard inside runCleanupAndDelete
// passed and the following call dereferenced nil. It runs in its own goroutine,
// so the panic was unrecoverable and killed the process.
func TestCleanupClient_NilStaysNilThroughTheInterface(t *testing.T) {
	var absent *kubernetes.StatusClient // what k8sForEnv returns for an unknown env

	if got := cleanupClient(absent); got != nil {
		t.Fatal("a nil client must yield a nil interface, otherwise the `k8s != nil` guard is bypassed and the call panics")
	}

	present := &kubernetes.StatusClient{}
	if got := cleanupClient(present); got == nil {
		t.Error("a real client must survive the conversion")
	}
}

func TestK8sForEnv_PerEnvClient(t *testing.T) {
	h := minimalHandlers()
	preprodClient := &kubernetes.StatusClient{}
	prodClient := &kubernetes.StatusClient{}
	h.k8sClients["preprod"] = preprodClient
	h.k8sClients["prod"] = prodClient

	if h.k8sForEnv("preprod") != preprodClient {
		t.Error("expected preprod-specific client")
	}
	if h.k8sForEnv("prod") != prodClient {
		t.Error("expected prod-specific client")
	}
}

// --- redirectWithFlash ---

func TestRedirectWithFlash_SetsHXRedirect(t *testing.T) {
	h := minimalHandlers()
	w := httptest.NewRecorder()
	h.redirectWithFlash(w, "/vclusters/myvc?env=preprod", "success", "Opération réussie")

	if w.Header().Get("HX-Redirect") != "/vclusters/myvc?env=preprod" {
		t.Errorf("HX-Redirect = %q", w.Header().Get("HX-Redirect"))
	}
}

func TestRedirectWithFlash_SetsCookie(t *testing.T) {
	h := minimalHandlers()
	w := httptest.NewRecorder()
	h.redirectWithFlash(w, "/", "success", "Done")

	cookies := w.Result().Cookies()
	var flashCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "flash" {
			flashCookie = c
			break
		}
	}
	if flashCookie == nil {
		t.Fatal("expected flash cookie, got none")
	}
	decoded, err := url.QueryUnescape(flashCookie.Value)
	if err != nil {
		t.Fatalf("cookie decode error: %v", err)
	}
	if !strings.HasPrefix(decoded, "success|") {
		t.Errorf("cookie value = %q, want prefix 'success|'", decoded)
	}
	if !strings.Contains(decoded, "Done") {
		t.Errorf("cookie value = %q, want to contain 'Done'", decoded)
	}
}

// --- actor ---

// TestActor_UsesPreferredUsernameNotDisplayName locks D3: the audit trail
// keys the actor on the stable login identifier, not the free-text display
// name two accounts can share.
func TestActor_UsesPreferredUsernameNotDisplayName(t *testing.T) {
	h := minimalHandlers()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(sessionCookie(map[string]interface{}{
		"name":               "Test Admin",
		"preferred_username": "testadmin",
	}))

	got := h.actor(r)
	if got.Username != "testadmin" {
		t.Errorf("Username = %q, want the stable preferred_username 'testadmin', not the display name", got.Username)
	}
}

func TestActor_FallsBackToNameWithoutPreferredUsername(t *testing.T) {
	h := minimalHandlers()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(sessionCookie(map[string]interface{}{"name": "admin"}))

	got := h.actor(r)
	if got.Username != "admin" {
		t.Errorf("Username = %q, want fallback to 'name'='admin'", got.Username)
	}
}

// --- requireAdmin ---

func TestRequireAdmin_ReturnsFalseAndForbiddenWhenNoSession(t *testing.T) {
	h := minimalHandlers()
	w := httptest.NewRecorder()
	// No auth cookie → not admin
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	result := h.requireAdmin(w, r)
	if result {
		t.Error("expected requireAdmin to return false when no session")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden, got %d", w.Code)
	}
}

func TestRequireAdmin_ToastRendered(t *testing.T) {
	h := minimalHandlers()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	h.requireAdmin(w, r)

	body := w.Body.String()
	// Our stub template renders "level:message"
	if !strings.Contains(body, "error") {
		t.Errorf("expected error toast in body, got: %q", body)
	}
}

// TestRequireAdmin_DoesNotRedirectToLogin verrouille l'arbitrage 401 vs 403 :
// un lecteur authentifié qui tente une action admin sait qui il est, il n'a
// juste pas le droit. Le rediriger vers /auth/login lui ferait croire qu'il
// est déconnecté, ce qui est faux et trompeur. Seul le middleware d'authen-
// tification (redirectToLogin, package auth) doit poser HX-Redirect ; un 403
// applicatif comme celui-ci reste un toast HTMX classique, sur place.
func TestRequireAdmin_DoesNotRedirectToLogin(t *testing.T) {
	h := minimalHandlers()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("HX-Request", "true")

	h.requireAdmin(w, r)

	if got := w.Header().Get("HX-Redirect"); got != "" {
		t.Errorf("HX-Redirect = %q, want none: un 403 (droits insuffisants) ne doit pas rediriger vers le login", got)
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}
