package auth

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// RateLimiter is a per-IP rate limiter middleware.
type RateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*ipLimiter
	r        rate.Limit
	b        int
}

// NewRateLimiter creates a rate limiter allowing r requests/sec with a burst of b per IP.
func NewRateLimiter(r rate.Limit, b int) *RateLimiter {
	rl := &RateLimiter{
		limiters: make(map[string]*ipLimiter),
		r:        r,
		b:        b,
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) get(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	entry, ok := rl.limiters[ip]
	if !ok {
		entry = &ipLimiter{limiter: rate.NewLimiter(rl.r, rl.b)}
		rl.limiters[ip] = entry
	}
	entry.lastSeen = time.Now()
	return entry.limiter
}

// cleanup removes idle entries every minute.
func (rl *RateLimiter) cleanup() {
	for range time.Tick(time.Minute) {
		rl.mu.Lock()
		for ip, entry := range rl.limiters {
			if time.Since(entry.lastSeen) > 5*time.Minute {
				delete(rl.limiters, ip)
			}
		}
		rl.mu.Unlock()
	}
}

// clientIP picks the address the rate limiter buckets on.
//
// The app sits behind exactly one reverse proxy (nginx today; Traefik is
// coming). That proxy is the only party allowed to append to
// X-Forwarded-For, and it always appends last — so the last entry is the
// address the proxy itself observed for this connection, which a client
// cannot forge. Everything before it is whatever the client claimed and
// carries no guarantee.
//
// Reading the *first* entry instead — what this used to do — hands the
// bucket key straight to the client: rotate that header and every request
// lands in a fresh, empty bucket, and the limiter never fires. Confirmed
// with 100 requests through a rotating XFF: 0 rejected, against 43/100 with
// a constant one (docs/recette-transverses.md, T6.3). Not exploitable here
// today only because nginx replaces the header outright instead of
// appending to it (verified against the running ingress config in the same
// recette) — that's infrastructure this code shouldn't have to lean on, and
// which the Traefik migration is about to change anyway.
//
// This still assumes the app is only reachable through that one proxy. A
// caller hitting it directly, bypassing the proxy entirely, could forge
// X-Forwarded-For freely — that's a network-topology guarantee (the Service
// isn't exposed except via the ingress), not something this function can
// verify from a request alone.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
			return last
		}
	}
	ip := r.RemoteAddr
	if colon := strings.LastIndex(ip, ":"); colon != -1 {
		ip = ip[:colon]
	}
	return ip
}

// Middleware returns an HTTP middleware that limits requests per IP.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.get(ip).Allow() {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
