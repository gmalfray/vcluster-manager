package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func loginFormRequest(username, password string) *http.Request {
	body := url.Values{"username": {username}, "password": {password}}.Encode()
	r := httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// decodeJWTPayload decodes the middle segment of a JWT without verifying the
// signature — good enough to inspect the claims a test just issued.
func decodeJWTPayload(t *testing.T, token string) map[string]interface{} {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT (expected 3 parts): %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding JWT payload: %v", err)
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshaling JWT payload: %v", err)
	}
	return claims
}

func TestLoginHandler_SessionCookieIsSecure(t *testing.T) {
	a := NewLocalAuth("correct-password", "test-secret", "", false)
	w := httptest.NewRecorder()
	r := loginFormRequest("admin", "correct-password")

	a.LoginHandler()(w, r)

	cookie := findCookie(w.Result().Cookies(), "session_token")
	if cookie == nil {
		t.Fatal("expected session_token cookie to be set")
	}
	// D7: r.TLS is always nil behind the ingress that terminates TLS, so
	// Secure:r.TLS!=nil never fires in production. Must be hardcoded true,
	// same as the OIDC callback cookie.
	if !cookie.Secure {
		t.Error("session_token cookie must be Secure")
	}
	if !cookie.HttpOnly {
		t.Error("session_token cookie must be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("want SameSite=Lax (matches the OIDC callback cookie), got %v", cookie.SameSite)
	}
}

func TestLoginHandler_WrongPasswordSetsNoCookie(t *testing.T) {
	a := NewLocalAuth("correct-password", "test-secret", "", false)
	w := httptest.NewRecorder()
	r := loginFormRequest("admin", "wrong-password")

	a.LoginHandler()(w, r)

	if cookie := findCookie(w.Result().Cookies(), "session_token"); cookie != nil {
		t.Error("expected no session_token cookie on failed login")
	}
	if w.Code != http.StatusSeeOther {
		t.Errorf("want 303 redirect to the login page with an error, got %d", w.Code)
	}
}

func TestLoginHandler_TokenCarriesPreferredUsername(t *testing.T) {
	a := NewLocalAuth("correct-password", "test-secret", "", false)
	w := httptest.NewRecorder()
	r := loginFormRequest("admin", "correct-password")

	a.LoginHandler()(w, r)

	cookie := findCookie(w.Result().Cookies(), "session_token")
	if cookie == nil {
		t.Fatal("expected session_token cookie to be set")
	}
	claims := decodeJWTPayload(t, cookie.Value)
	// D3: the audit trail keys on preferred_username, not the display name
	// ("name"). Without this claim, ActorUsername falls back to "name" and
	// the local admin account is indistinguishable from anyone else who
	// happens to display as "admin".
	if got, _ := claims["preferred_username"].(string); got != "admin" {
		t.Errorf("want preferred_username claim 'admin', got %q", got)
	}
}

func TestLoginPageHandler_SetsCSRFCookieAndFormField(t *testing.T) {
	a := NewLocalAuth("correct-password", "test-secret", "../../web/templates", false)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)

	a.LoginPageHandler()(w, r)

	cookie := findCookie(w.Result().Cookies(), csrfCookieName)
	if cookie == nil {
		t.Fatal("expected csrf_token cookie on the login page — without it POST /auth/local/login always fails CSRF on a fresh browser")
	}
	if cookie.Value == "" {
		t.Fatal("csrf_token cookie value must not be empty")
	}
	body := w.Body.String()
	if !strings.Contains(body, `name="_csrf" value="`+cookie.Value+`"`) {
		t.Errorf("expected the login form to carry a hidden _csrf field matching the cookie value; body:\n%s", body)
	}
}

func TestLoginPageHandler_ReusesExistingCSRFCookie(t *testing.T) {
	a := NewLocalAuth("correct-password", "test-secret", "../../web/templates", false)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "already-there"})

	a.LoginPageHandler()(w, r)

	if cookie := findCookie(w.Result().Cookies(), csrfCookieName); cookie != nil {
		t.Error("should not overwrite an existing csrf_token cookie")
	}
	if !strings.Contains(w.Body.String(), `name="_csrf" value="already-there"`) {
		t.Error("expected the form to carry the existing csrf_token value")
	}
}

// TestLoginRoute_CSRFAndRateLimit exercises the wiring cmd/server/main.go is
// responsible for: POST /auth/local/login behind auth.CSRFMiddleware and a
// dedicated RateLimiter, the same way it's assembled there. This is the
// closest a package-level test can get to proving D6 without spinning up the
// real server.
func TestLoginRoute_CSRFAndRateLimit(t *testing.T) {
	a := NewLocalAuth("correct-password", "test-secret", "../../web/templates", false)
	limiter := NewRateLimiter(1, 5)
	route := limiter.Middleware(CSRFMiddleware(a.LoginHandler()))

	// No CSRF cookie at all: must be rejected before credentials are ever checked.
	w := httptest.NewRecorder()
	r := loginFormRequest("admin", "correct-password")
	route.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 without a CSRF cookie, got %d", w.Code)
	}
	if findCookie(w.Result().Cookies(), "session_token") != nil {
		t.Error("must not authenticate a request that failed CSRF")
	}

	// CSRF cookie present but no token submitted: still rejected.
	w = httptest.NewRecorder()
	r = loginFormRequest("admin", "correct-password")
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok"})
	route.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 with a CSRF cookie but no submitted token, got %d", w.Code)
	}

	// Matching cookie and form field: passes CSRF, credentials get checked.
	body := url.Values{"username": {"admin"}, "password": {"correct-password"}, "_csrf": {"tok"}}.Encode()
	r = httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok"})
	w = httptest.NewRecorder()
	route.ServeHTTP(w, r)
	if findCookie(w.Result().Cookies(), "session_token") == nil {
		t.Error("expected a session_token cookie once CSRF and credentials both pass")
	}
}

// TestLoginRoute_RateLimited proves the dedicated limiter actually caps
// attempts on this route, independent of the global 20 req/s one — the
// brute-force half of D6.
func TestLoginRoute_RateLimited(t *testing.T) {
	a := NewLocalAuth("correct-password", "test-secret", "../../web/templates", false)
	limiter := NewRateLimiter(1, 3) // burst of 3, matches the shape used in main.go
	route := limiter.Middleware(CSRFMiddleware(a.LoginHandler()))

	var got429 bool
	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		r := loginFormRequest("admin", "wrong-password")
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok"})
		r.Header.Set(csrfHeaderName, "tok")
		route.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("expected the dedicated login limiter to eventually reject a burst of attempts")
	}
}
