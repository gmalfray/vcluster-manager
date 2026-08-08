package auth

import (
	"crypto/subtle"
	"html/template"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type LocalAuth struct {
	password    string
	jwtSecret   []byte
	templateDir string
	oidcEnabled bool
}

func NewLocalAuth(password, jwtSecret, templateDir string, oidcEnabled bool) *LocalAuth {
	return &LocalAuth{
		password:    password,
		jwtSecret:   []byte(jwtSecret),
		templateDir: templateDir,
		oidcEnabled: oidcEnabled,
	}
}

func (a *LocalAuth) LoginPageHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := template.ParseFiles(
			filepath.Join(a.templateDir, "login.html"),
		)
		if err != nil {
			slog.Error("login template parse failed", "err", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		// The login page lives outside the protected mux (no CSRFMiddleware), so
		// it has to pose its own csrf_token cookie and hand the value to the
		// form — otherwise POST /auth/local/login would always fail the CSRF
		// check with "manquant" on a fresh browser.
		csrfToken := EnsureCSRFCookie(w, r)
		data := map[string]interface{}{
			"OIDCEnabled": a.oidcEnabled,
			"Error":       r.URL.Query().Get("error"),
			"CSRFToken":   csrfToken,
		}
		if err := tmpl.Execute(w, data); err != nil {
			slog.Warn("login template execute failed", "err", err)
		}
	}
}

func (a *LocalAuth) LoginHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := r.FormValue("username")
		password := r.FormValue("password")

		if username != "admin" || subtle.ConstantTimeCompare([]byte(password), []byte(a.password)) != 1 {
			http.Redirect(w, r, "/auth/login?error=Identifiants+invalides", http.StatusSeeOther)
			return
		}

		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"name":               "admin",
			"preferred_username": "admin",
			"email":              "admin@local",
			"groups":             []string{"admin"},
			"exp":                time.Now().Add(8 * time.Hour).Unix(),
			"iat":                time.Now().Unix(),
			"iss":                "vcluster-manager-local",
		})

		tokenString, err := token.SignedString(a.jwtSecret)
		if err != nil {
			slog.Error("JWT signing failed", "err", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "session_token",
			Value:    tokenString,
			Path:     "/",
			MaxAge:   int(8 * time.Hour / time.Second),
			HttpOnly: true,
			// TLS is always terminated at the ingress, so r.TLS is nil even in
			// production — this dropped the Secure flag on every real request.
			// Hardcoded true to match the OIDC callback cookie (oidc.go); the app
			// assumes it's always served over HTTPS, same as that code path.
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})

		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (a *LocalAuth) VerifyToken(tokenString string) error {
	_, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return a.jwtSecret, nil
	})
	return err
}
