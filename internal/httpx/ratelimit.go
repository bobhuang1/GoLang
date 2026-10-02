package httpx

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// RateLimiter is a small fixed-window, per-client-IP limiter for the credential
// endpoints (login, 2FA, password reset). It is in-memory, so with several instances
// each one counts separately; put a shared limiter (gateway, Redis) in front for that.
type RateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*windowCount
}

type windowCount struct {
	start time.Time
	count int
}

// NewRateLimiter allows `limit` requests per client IP per `window`.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window, hits: make(map[string]*windowCount)}
}

// Allow records one request from ip and reports whether it is within the limit.
func (l *RateLimiter) Allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.hits) > 10000 {
		for k, v := range l.hits {
			if now.Sub(v.start) >= l.window {
				delete(l.hits, k)
			}
		}
	}

	w, ok := l.hits[ip]
	if !ok || now.Sub(w.start) >= l.window {
		l.hits[ip] = &windowCount{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// Middleware answers 429 once a client IP exceeds the limit.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		if !l.Allow(ip, time.Now()) {
			w.Header().Set("Retry-After", "60")
			WriteJSON(w, http.StatusTooManyRequests, Envelope{
				Error: EnvelopeError{Code: "rate_limited", Message: "too many attempts, try again later"},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
