package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func TestCSRF_GetSetsCookie(t *testing.T) {
	h := CSRFMiddleware(okHandler)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var csrfCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == csrfCookieName {
			csrfCookie = c
			break
		}
	}
	if csrfCookie == nil {
		t.Fatal("expected csrf_token cookie to be set on GET")
	}
	if len(csrfCookie.Value) == 0 {
		t.Error("csrf_token cookie value should not be empty")
	}
	if csrfCookie.SameSite != http.SameSiteStrictMode {
		t.Error("csrf_token cookie should be SameSite=Strict")
	}
	if csrfCookie.HttpOnly {
		t.Error("csrf_token cookie must not be HttpOnly (JS needs to read it for HTMX)")
	}
}

func TestCSRF_GetDoesNotRefreshExistingCookie(t *testing.T) {
	h := CSRFMiddleware(okHandler)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "existing-token"})

	h.ServeHTTP(w, r)

	// No new cookie should be set because one already exists
	for _, c := range w.Result().Cookies() {
		if c.Name == csrfCookieName {
			t.Error("should not reset csrf_token cookie when one already exists")
		}
	}
}

func TestCSRF_PostWithValidHeader(t *testing.T) {
	h := CSRFMiddleware(okHandler)
	token := "abc123validtoken"

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	r.Header.Set(csrfHeaderName, token)

	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestCSRF_PostWithValidFormField(t *testing.T) {
	h := CSRFMiddleware(okHandler)
	token := "abc123validtoken"

	body := "_csrf=" + token
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})

	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with valid form field, got %d", w.Code)
	}
}

func TestCSRF_PostWithNoCookie(t *testing.T) {
	h := CSRFMiddleware(okHandler)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set(csrfHeaderName, "some-token")
	// No cookie

	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 when no cookie, got %d", w.Code)
	}
}

func TestCSRF_PostWithNoToken(t *testing.T) {
	h := CSRFMiddleware(okHandler)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "abc123"})
	// No header, no form field

	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 when token not submitted, got %d", w.Code)
	}
}

func TestCSRF_PostWithMismatchedToken(t *testing.T) {
	h := CSRFMiddleware(okHandler)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "cookie-token"})
	r.Header.Set(csrfHeaderName, "different-token")

	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 when tokens don't match, got %d", w.Code)
	}
}

func TestCSRF_PutIsProtected(t *testing.T) {
	h := CSRFMiddleware(okHandler)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "token"})
	// No submitted token

	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("PUT without CSRF token should return 403, got %d", w.Code)
	}
}

func TestCSRF_HeadPassesThrough(t *testing.T) {
	h := CSRFMiddleware(okHandler)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodHead, "/", nil)

	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("HEAD should pass through, got %d", w.Code)
	}
}

func TestGenerateCSRFToken_IsHex64Chars(t *testing.T) {
	token := generateCSRFToken()
	if len(token) != 64 {
		t.Errorf("expected 64 hex chars (32 bytes), got %d: %q", len(token), token)
	}
	for _, c := range token {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-hex character in token: %q", string(c))
		}
	}
}

func TestGenerateCSRFToken_IsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		token := generateCSRFToken()
		if seen[token] {
			t.Fatalf("generated duplicate token: %q", token)
		}
		seen[token] = true
	}
}
