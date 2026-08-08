package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/time/rate"
)

func TestClientIP_NoForwardedForUsesRemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:54321"
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("want 203.0.113.9, got %q", got)
	}
}

func TestClientIP_SingleForwardedForValue(t *testing.T) {
	// This is what nginx does today: it replaces the header outright with the
	// real client address (proxy_set_header X-Forwarded-For $remote_addr),
	// so there's only one entry and it's trustworthy.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("want 203.0.113.9, got %q", got)
	}
}

func TestClientIP_TakesLastEntryNotFirst(t *testing.T) {
	// A proxy appends its own view of the peer to the end of the header —
	// anything before that is whatever the client claimed. The first entry
	// here is attacker-controlled fiction; the last is what the (hypothetical)
	// proxy actually observed.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("want the last hop 203.0.113.9, got %q — first-entry trust is exactly the D10 bug", got)
	}
}

func TestClientIP_RotatingClaimedPrefixStillBucketsTogether(t *testing.T) {
	// This is the exploit measured in the recette: rotate the header, get a
	// fresh bucket every time, the limiter never fires. With the last-hop
	// value trusted instead, a rotating prefix in front of a stable proxy hop
	// still resolves to the same client IP.
	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r1.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.9")
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("X-Forwarded-For", "198.51.100.2, 203.0.113.9")

	got1, got2 := clientIP(r1), clientIP(r2)
	if got1 != got2 {
		t.Errorf("rotating the claimed prefix should not change the bucket: got %q and %q", got1, got2)
	}
}

func TestClientIP_TrailingCommaFallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, ")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("want fallback to RemoteAddr 203.0.113.9 for an empty trailing entry, got %q", got)
	}
}

// TestRateLimiter_RotatingClaimedPrefixStillTrips reproduces D10 end to end
// through the real middleware: a client that rotates the part of
// X-Forwarded-For it controls, behind a stable proxy hop, still gets rate
// limited. Before the fix this sent every request to a fresh, empty bucket
// and 429 never happened.
func TestRateLimiter_RotatingClaimedPrefixStillTrips(t *testing.T) {
	rl := NewRateLimiter(rate.Limit(1), 3)
	handler := rl.Middleware(okHandler)

	var got429 bool
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d, 203.0.113.9", i))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("expected the limiter to eventually reject requests sharing the same trusted last hop, got none")
	}
}
