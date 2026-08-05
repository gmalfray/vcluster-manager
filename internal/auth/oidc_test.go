package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// makeTestJWT encodes a minimal JWT (header.payload.fakesig) for testing.
// The signature is not verified by IsAdmin or UserFromRequest.
func makeTestJWT(claims map[string]interface{}) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadJSON, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	return header + "." + payload + ".fakesig"
}

func requestWithCookie(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "session_token", Value: token})
	return r
}

// --- splitJWT ---

func TestSplitJWT_ValidThreePart(t *testing.T) {
	parts := splitJWT("a.b.c")
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	if parts[0] != "a" || parts[1] != "b" || parts[2] != "c" {
		t.Errorf("unexpected parts: %v", parts)
	}
}

func TestSplitJWT_NoDots(t *testing.T) {
	parts := splitJWT("nodots")
	if len(parts) != 1 || parts[0] != "nodots" {
		t.Errorf("want [nodots], got %v", parts)
	}
}

func TestSplitJWT_Empty(t *testing.T) {
	parts := splitJWT("")
	if len(parts) != 1 {
		t.Fatalf("want 1 part for empty string, got %d", len(parts))
	}
}

// --- generateState ---

func TestGenerateState_NonEmpty(t *testing.T) {
	s := generateState()
	if s == "" {
		t.Fatal("generateState returned empty string")
	}
}

func TestGenerateState_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		s := generateState()
		if seen[s] {
			t.Fatalf("duplicate state generated: %s", s)
		}
		seen[s] = true
	}
}

// --- IsAdmin ---

func TestIsAdmin_LocalIssuer(t *testing.T) {
	token := makeTestJWT(map[string]interface{}{
		"iss":    "vcluster-manager-local",
		"name":   "admin",
		"groups": []string{},
	})
	r := requestWithCookie(token)
	if !IsAdmin(r) {
		t.Fatal("want admin=true for local issuer")
	}
}

func TestIsAdmin_AdminGroup(t *testing.T) {
	SetAdminGroups([]string{"ops"})
	t.Cleanup(func() { adminGroups = map[string]bool{"platform-admins": true, "ops": true} })

	token := makeTestJWT(map[string]interface{}{
		"iss":    "https://idp.example.com",
		"groups": []string{"devs", "ops"},
	})
	r := requestWithCookie(token)
	if !IsAdmin(r) {
		t.Fatal("want admin=true for user in ops group")
	}
}

func TestIsAdmin_NotInAdminGroup(t *testing.T) {
	SetAdminGroups([]string{"ops"})
	t.Cleanup(func() { adminGroups = map[string]bool{"platform-admins": true, "ops": true} })

	token := makeTestJWT(map[string]interface{}{
		"iss":    "https://idp.example.com",
		"groups": []string{"devs"},
	})
	r := requestWithCookie(token)
	if IsAdmin(r) {
		t.Fatal("want admin=false for user not in ops group")
	}
}

func TestIsAdmin_NoCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if IsAdmin(r) {
		t.Fatal("want admin=false for request without cookie")
	}
}

// --- UserFromRequest ---

func TestUserFromRequest_ValidJWT(t *testing.T) {
	token := makeTestJWT(map[string]interface{}{
		"email": "alice@example.com",
		"name":  "Alice",
	})
	r := requestWithCookie(token)
	claims := UserFromRequest(r)
	if auth, _ := claims["authenticated"].(bool); !auth {
		t.Error("want authenticated=true")
	}
	if email, _ := claims["email"].(string); email != "alice@example.com" {
		t.Errorf("want email=alice@example.com, got %q", email)
	}
}

func TestUserFromRequest_NoCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	claims := UserFromRequest(r)
	if auth, _ := claims["authenticated"].(bool); auth {
		t.Error("want authenticated=false for request without cookie")
	}
}

func TestUserFromRequest_MalformedToken(t *testing.T) {
	r := requestWithCookie("not-a-jwt")
	claims := UserFromRequest(r)
	if auth, _ := claims["authenticated"].(bool); auth {
		t.Error("want authenticated=false for malformed token")
	}
}

// --- SetAdminGroups ---

func TestSetAdminGroups_Configures(t *testing.T) {
	orig := adminGroups
	t.Cleanup(func() { adminGroups = orig })

	SetAdminGroups([]string{"sre", "platform"})
	if !adminGroups["sre"] || !adminGroups["platform"] {
		t.Error("SetAdminGroups did not set expected groups")
	}
	if adminGroups["it"] {
		t.Error("old group 'it' should not be present after SetAdminGroups")
	}
}

func TestSetAdminGroups_EmptySliceKeepsDefaults(t *testing.T) {
	orig := adminGroups
	t.Cleanup(func() { adminGroups = orig })

	SetAdminGroups([]string{})
	// Empty input should keep previous configuration
	if len(adminGroups) == 0 {
		t.Error("SetAdminGroups with empty slice should not clear admin groups")
	}
}

// --- redirectToLogin / CombinedMiddleware: session expiry on HTMX ----------
//
// A bare 307 to /auth/login is fine for a normal navigation, but an HTMX
// poll or click swaps the response body into whatever fragment issued the
// request — the login page's HTML ends up injected into a status card
// instead of taking over the tab. HX-Redirect tells htmx to navigate the
// whole page instead.

func TestRedirectToLogin_HTMXRequestGetsHXRedirectHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/vclusters/demo/status", nil)
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	redirectToLogin(w, r)

	if got := w.Header().Get("HX-Redirect"); got != "/auth/login" {
		t.Errorf("HX-Redirect header = %q, want /auth/login", got)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (no full-page redirect for HTMX)", w.Code, http.StatusOK)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("Location header = %q, want none (HX-Redirect replaces it)", loc)
	}
}

func TestRedirectToLogin_ClassicRequestUsesHTTPRedirect(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/vclusters/demo", nil)
	w := httptest.NewRecorder()

	redirectToLogin(w, r)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTemporaryRedirect)
	}
	if loc := w.Header().Get("Location"); loc != "/auth/login" {
		t.Errorf("Location header = %q, want /auth/login", loc)
	}
	if got := w.Header().Get("HX-Redirect"); got != "" {
		t.Errorf("HX-Redirect header = %q, want none for a non-HTMX request", got)
	}
}

func TestCombinedMiddleware_HTMXRequestWithoutCookieGetsHXRedirect(t *testing.T) {
	mw := CombinedMiddleware(nil, nil)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	r := httptest.NewRequest(http.MethodGet, "/api/vclusters/demo/status", nil)
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if called {
		t.Error("the wrapped handler must not run for an unauthenticated request")
	}
	if got := w.Header().Get("HX-Redirect"); got != "/auth/login" {
		t.Errorf("HX-Redirect header = %q, want /auth/login", got)
	}
}

func TestCombinedMiddleware_HTMXRequestWithInvalidTokenGetsHXRedirect(t *testing.T) {
	// A cookie is present (session expired, say) but neither auth backend
	// can verify it — exercises the second redirectToLogin call site, after
	// both verification attempts fail.
	localAuth := NewLocalAuth("admin-pw", "test-secret", "", false)
	mw := CombinedMiddleware(nil, localAuth)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the wrapped handler must not run for an invalid token")
	}))

	r := httptest.NewRequest(http.MethodGet, "/api/vclusters/demo/status", nil)
	r.AddCookie(&http.Cookie{Name: "session_token", Value: "not-a-valid-jwt"})
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if got := w.Header().Get("HX-Redirect"); got != "/auth/login" {
		t.Errorf("HX-Redirect header = %q, want /auth/login", got)
	}
}

func TestCombinedMiddleware_ClassicRequestWithoutCookieGetsHTTPRedirect(t *testing.T) {
	mw := CombinedMiddleware(nil, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the wrapped handler must not run for an unauthenticated request")
	}))

	r := httptest.NewRequest(http.MethodGet, "/vclusters/demo", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTemporaryRedirect)
	}
	if got := w.Header().Get("HX-Redirect"); got != "" {
		t.Errorf("HX-Redirect header = %q, want none for a non-HTMX request", got)
	}
}

func TestCombinedMiddleware_ValidLocalTokenPassesThrough(t *testing.T) {
	localAuth := NewLocalAuth("admin-pw", "test-secret", "", false)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "vcluster-manager-local",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tokenString, err := token.SignedString(localAuth.jwtSecret)
	if err != nil {
		t.Fatalf("unexpected error signing test token: %v", err)
	}

	mw := CombinedMiddleware(nil, localAuth)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	r := httptest.NewRequest(http.MethodGet, "/vclusters/demo", nil)
	r.AddCookie(&http.Cookie{Name: "session_token", Value: tokenString})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if !called {
		t.Error("expected the wrapped handler to run for a valid token")
	}
}
