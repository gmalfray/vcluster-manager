package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type OIDCAuth struct {
	provider *oidc.Provider
	config   oauth2.Config
	verifier *oidc.IDTokenVerifier
}

type Claims struct {
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
}

func NewOIDCAuth(issuerURL, clientID, clientSecret, redirectURL string) (*OIDCAuth, error) {
	ctx := context.Background()
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("creating OIDC provider: %w", err)
	}

	config := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: clientID})

	return &OIDCAuth{
		provider: provider,
		config:   config,
		verifier: verifier,
	}, nil
}

// Middleware protects routes with OIDC auth.
func (a *OIDCAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session_token")
		if err != nil {
			redirectToLogin(w, r)
			return
		}

		_, err = a.verifier.Verify(r.Context(), cookie.Value)
		if err != nil {
			redirectToLogin(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// redirectToLogin sends an unauthenticated request to /auth/login. An HTMX
// request (poll, form submit, click) gets an HX-Redirect header instead of a
// classic 307: swapped into a fragment, the login page's HTML would land
// inside whatever partial was polling (e.g. the login form floating in the
// status card) instead of taking over the browser tab. HX-Redirect makes
// htmx do a full-page navigation client-side, same destination either way.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/auth/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/auth/login", http.StatusTemporaryRedirect)
}

// LoginHandler initiates the OIDC flow.
func (a *OIDCAuth) LoginHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := generateState()
		if err != nil {
			// Pas de repli : sans state imprévisible, la connexion ne doit pas
			// avoir lieu du tout.
			slog.Error("état OIDC non généré, connexion refusée", "err", err)
			http.Error(w, "connexion indisponible", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "oauth_state",
			Value:    state,
			Path:     "/",
			MaxAge:   300,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, a.config.AuthCodeURL(state), http.StatusTemporaryRedirect)
	}
}

// CallbackHandler handles the OIDC callback.
func (a *OIDCAuth) CallbackHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stateCookie, err := r.Cookie("oauth_state")
		if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			return
		}

		token, err := a.config.Exchange(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			slog.Error("OIDC token exchange failed", "err", err)
			http.Error(w, "Authentication failed", http.StatusInternalServerError)
			return
		}

		rawIDToken, ok := token.Extra("id_token").(string)
		if !ok {
			http.Error(w, "No id_token in response", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "session_token",
			Value:    rawIDToken,
			Path:     "/",
			MaxAge:   int(8 * time.Hour / time.Second),
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})

		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
	}
}

// LogoutHandler clears the session.
func (a *OIDCAuth) LogoutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     "session_token",
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
		})
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
	}
}

// GetClaims extracts claims from the session cookie.
func (a *OIDCAuth) GetClaims(r *http.Request) (*Claims, error) {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return nil, err
	}
	idToken, err := a.verifier.Verify(r.Context(), cookie.Value)
	if err != nil {
		return nil, err
	}
	var claims Claims
	if err := idToken.Claims(&claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

// CombinedMiddleware tries OIDC verification first, then local JWT.
func CombinedMiddleware(oidcAuth *OIDCAuth, localAuth *LocalAuth) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie("session_token")
			if err != nil {
				redirectToLogin(w, r)
				return
			}

			// Try OIDC verification
			if oidcAuth != nil {
				if _, err := oidcAuth.verifier.Verify(r.Context(), cookie.Value); err == nil {
					next.ServeHTTP(w, r)
					return
				}
			}

			// Try local JWT verification
			if localAuth != nil {
				if err := localAuth.VerifyToken(cookie.Value); err == nil {
					next.ServeHTTP(w, r)
					return
				}
			}

			redirectToLogin(w, r)
		})
	}
}

// NoopMiddleware is used in dev mode (no auth).
func NoopMiddleware(next http.Handler) http.Handler {
	return next
}

// generateState produit le paramètre `state` de l'OIDC, celui qui lie la
// redirection au navigateur qui l'a demandée.
//
// L'erreur de rand.Read était ignorée. En pratique crypto/rand n'échoue jamais
// sur Linux, mais le code était écrit pour rendre silencieusement un buffer de
// zéros : un state CONSTANT, identique pour tout le monde, donc un contrôle
// anti-CSRF qui ne contrôle plus rien — n'importe qui peut forger un callback
// dont le state correspond. Échouer bruyamment est la seule issue acceptable
// pour une valeur dont toute la valeur est d'être imprévisible.
func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("génération du state OIDC : %w", err)
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// ActorUsername returns the stable identifier to record for the current
// session — preferred_username (the login name) rather than the display
// name. Keycloak's "name" claim and the local JWT's "name" claim are both
// free-text display names: two accounts can share one ("Test Admin"), and a
// user can change theirs in Keycloak, which would silently rewrite who past
// audit lines look like they belonged to. preferred_username is what stays
// stable. Falls back to "name" for tokens that don't carry it (there
// shouldn't be any once local.go sets it, but this keeps a signed-in user
// audited under something rather than "unknown" if one slips through).
func ActorUsername(r *http.Request) string {
	user := UserFromRequest(r)
	if u, ok := user["preferred_username"].(string); ok && u != "" {
		return u
	}
	u, _ := user["name"].(string)
	return u
}

// UserFromRequest returns the authenticated user's claims as a JSON-safe map for use in templates.
func UserFromRequest(r *http.Request) map[string]interface{} {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return map[string]interface{}{"authenticated": false}
	}
	// Decode JWT payload without verification (for display only)
	parts := splitJWT(cookie.Value)
	if len(parts) < 2 {
		return map[string]interface{}{"authenticated": false}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]interface{}{"authenticated": false}
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return map[string]interface{}{"authenticated": false}
	}
	claims["authenticated"] = true
	return claims
}

// adminGroups defines which OIDC groups have admin (write) access.
// Populated at startup via SetAdminGroups; defaults to {"platform-admins","ops"} if never set.
var adminGroups = map[string]bool{"platform-admins": true, "ops": true}

// SetAdminGroups configures which OIDC group names grant admin (write) access.
// Call once at startup with the parsed ADMIN_GROUPS env var.
// If groups is empty the default {"platform-admins","ops"} is kept.
func SetAdminGroups(groups []string) {
	m := make(map[string]bool, len(groups))
	for _, g := range groups {
		if g != "" {
			m[g] = true
		}
	}
	if len(m) > 0 {
		adminGroups = m
	}
}

// IsAdmin checks if the current user has admin privileges.
// Admin access is granted if:
// - The JWT issuer is "vcluster-manager-local" (local admin login)
// - The user belongs to one of the adminGroups
//
// Invariant: this reads the token's claims without checking its signature —
// it trusts that CombinedMiddleware already verified it earlier in the
// request. Only call IsAdmin on a request that went through that middleware;
// every mutating route MUST sit behind authMiddleware (see cmd/server/main.go)
// or this check is worthless. Known debt: claims should instead be verified
// once by the middleware and passed down via request context, so nothing
// downstream can call IsAdmin on an unverified request by mistake.
func IsAdmin(r *http.Request) bool {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return false
	}
	parts := splitJWT(cookie.Value)
	if len(parts) < 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return false
	}
	// Local admin login is always admin
	if iss, ok := claims["iss"].(string); ok && iss == "vcluster-manager-local" {
		return true
	}
	// Check OIDC groups
	if groups, ok := claims["groups"].([]interface{}); ok {
		for _, g := range groups {
			if gs, ok := g.(string); ok && adminGroups[gs] {
				return true
			}
		}
	}
	return false
}

func splitJWT(token string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	parts = append(parts, token[start:])
	return parts
}
