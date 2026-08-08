package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

const (
	csrfCookieName = "csrf_token"
	csrfHeaderName = "X-CSRF-Token"
	csrfFormField  = "_csrf"
)

// generateCSRFToken creates a cryptographically secure random 32-byte hex token.
func generateCSRFToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("csrf: rand.Read failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// CSRFMiddleware implements double-submit cookie CSRF protection.
//
// On safe methods (GET, HEAD, OPTIONS): ensures the csrf_token cookie is present,
// creating it if absent. The cookie is SameSite=Strict and readable by JS (HttpOnly=false)
// so HTMX can read it and send it as the X-CSRF-Token header.
//
// On unsafe methods (POST, PUT, DELETE, PATCH): validates that the submitted token
// (from X-CSRF-Token header for HTMX, or _csrf form field for classic forms)
// matches the cookie value.
func CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// Ensure the cookie exists so HTMX can read it after the first page load.
			EnsureCSRFCookie(w, r)
			next.ServeHTTP(w, r)

		default:
			// Validate the token on any state-changing method.
			cookieToken := csrfCookieValue(r)
			if cookieToken == "" {
				http.Error(w, "CSRF token manquant", http.StatusForbidden)
				return
			}

			// HTMX sends the token in the header; classic forms send it in a hidden field.
			submitted := r.Header.Get(csrfHeaderName)
			if submitted == "" {
				if err := r.ParseForm(); err == nil {
					submitted = r.FormValue(csrfFormField)
				}
			}

			if submitted == "" || submitted != cookieToken {
				http.Error(w, "Token CSRF invalide", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		}
	})
}

// EnsureCSRFCookie makes sure the csrf_token cookie is set on the response,
// creating one if the request doesn't already carry it, and returns its
// value. Shared by CSRFMiddleware (every GET) and the local login page,
// which renders outside the protected mux and needs to hand the token to its
// form.
func EnsureCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if token := csrfCookieValue(r); token != "" {
		return token
	}
	token := generateCSRFToken()
	http.SetCookie(w, newCSRFCookie(token))
	return token
}

func csrfCookieValue(r *http.Request) string {
	c, err := r.Cookie(csrfCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func newCSRFCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   86400, // 24h — refreshed on every GET
		SameSite: http.SameSiteStrictMode,
		HttpOnly: false, // Must be readable by JS for HTMX to include it in headers
	}
}
