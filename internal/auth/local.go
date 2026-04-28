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
		data := map[string]interface{}{
			"OIDCEnabled": a.oidcEnabled,
			"Error":       r.URL.Query().Get("error"),
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
			"name":   "admin",
			"email":  "admin@local",
			"groups": []string{"admin"},
			"exp":    time.Now().Add(8 * time.Hour).Unix(),
			"iat":    time.Now().Unix(),
			"iss":    "vcluster-manager-local",
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
			Secure:   r.TLS != nil,
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
