package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
	t.Cleanup(func() { adminGroups = map[string]bool{"exploit": true, "it": true} })

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
	t.Cleanup(func() { adminGroups = map[string]bool{"exploit": true, "it": true} })

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
