package audit

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func requestWithSession(claims map[string]interface{}) *http.Request {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadJSON, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "session_token", Value: header + "." + payload + ".fakesig"})
	return r
}

func withCapturedLog(fn func()) string {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// TestLog_UsesPreferredUsernameNotDisplayName locks D3 at the transport-coupled
// entry point (audit.Log), the one every handler still not migrated to
// models.Actor calls directly. auth.ActorUsername already has its own tests,
// but nothing outside this package would fail if Log stopped calling it and
// went back to reading the "name" claim — this closes that gap.
func TestLog_UsesPreferredUsernameNotDisplayName(t *testing.T) {
	r := requestWithSession(map[string]interface{}{
		"name":               "Test Admin",
		"preferred_username": "testadmin",
	})

	out := withCapturedLog(func() {
		Log(r, "some-action", "demo", "preprod")
	})

	if !strings.Contains(out, "user=testadmin") {
		t.Errorf("expected the audit line to key on preferred_username 'testadmin', got:\n%s", out)
	}
	if strings.Contains(out, "user=\"Test Admin\"") || strings.Contains(out, "user=Test") {
		t.Errorf("audit line must not key on the display name, got:\n%s", out)
	}
}

func TestLog_FallsBackToNameWithoutPreferredUsername(t *testing.T) {
	r := requestWithSession(map[string]interface{}{"name": "admin"})

	out := withCapturedLog(func() {
		Log(r, "some-action", "demo", "preprod")
	})

	if !strings.Contains(out, "user=admin") {
		t.Errorf("expected fallback to name 'admin', got:\n%s", out)
	}
}

func TestLog_NoSessionRecordsUnknown(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	out := withCapturedLog(func() {
		Log(r, "some-action", "demo", "preprod")
	})

	if !strings.Contains(out, "user=unknown") {
		t.Errorf("expected 'unknown' for a request without a session, got:\n%s", out)
	}
}
