package httpx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// IdempotencyGuard deduplicates requests carrying an Idempotency-Key header.
//
// It remembers the response produced for a given key+path for a short window,
// so a client retry (e.g. a double-charge attempt or a network hiccup) receives
// the exact same result instead of executing the side effect twice. This is a
// best-effort in-memory layer; the durable guarantee lives in the database
// unique constraints on the idempotency keys.
type IdempotencyGuard struct {
	mu       sync.Mutex
	store    map[string]*cachedResponse
	inFlight map[string]bool
	ttl      time.Duration
	maxEntry int
}

type cachedResponse struct {
	body     []byte
	status   int
	expires  time.Time
	lastSeen time.Time
}

// NewIdempotencyGuard builds a guard with the given cache TTL and size cap.
func NewIdempotencyGuard(ttl time.Duration, maxEntry int) *IdempotencyGuard {
	g := &IdempotencyGuard{
		store:    make(map[string]*cachedResponse),
		inFlight: make(map[string]bool),
		ttl:      ttl,
		maxEntry: maxEntry,
	}
	go g.reaper()
	return g
}

// Middleware reads the Idempotency-Key header. When present, the first request
// is executed and its reply is cached; later identical requests replay it.
func (g *IdempotencyGuard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		// Only state-changing POSTs are replayed; a GET carrying the header must always
		// see fresh data.
		if key == "" || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		// Keys are chosen by clients, so the cache is partitioned by caller: without the
		// credential in the key, a second customer reusing a key would be served the
		// first customer's cached response. The token is hashed, never stored as-is.
		caller := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		cacheKey := r.Method + " " + r.URL.Path + " " + hex.EncodeToString(caller[:]) + " " + key

		g.mu.Lock()
		if g.inFlight[cacheKey] {
			g.mu.Unlock()
			WriteError(w, Conflict("a request with this Idempotency-Key is still being processed"))
			return
		}
		if cached, ok := g.store[cacheKey]; ok && time.Now().Before(cached.expires) {
			cached.lastSeen = time.Now()
			g.mu.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Idempotent-Replay", "true")
			w.WriteHeader(cached.status)
			_, _ = w.Write(cached.body)
			return
		}
		g.inFlight[cacheKey] = true
		g.mu.Unlock()

		rec := &captureWriter{header: make(http.Header), status: 200}
		func() {
			// Release the in-flight marker even if the handler panics.
			defer func() {
				g.mu.Lock()
				delete(g.inFlight, cacheKey)
				g.mu.Unlock()
			}()
			next.ServeHTTP(rec, r)
		}()

		// Only successful outcomes are replayed. Caching a 4xx/5xx would hand a client
		// that retries after a transient failure the same failure for the whole TTL.
		if rec.status >= 200 && rec.status < 300 {
			g.mu.Lock()
			g.store[cacheKey] = &cachedResponse{
				body:     rec.body.Bytes(),
				status:   rec.status,
				expires:  time.Now().Add(g.ttl),
				lastSeen: time.Now(),
			}
			if len(g.store) > g.maxEntry {
				g.evictLocked()
			}
			g.mu.Unlock()
		}

		for k, vs := range rec.header {
			w.Header()[k] = vs
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	})
}

func (g *IdempotencyGuard) evictLocked() {
	oldest := time.Now()
	var victim string
	for k, v := range g.store {
		if v.lastSeen.Before(oldest) {
			oldest = v.lastSeen
			victim = k
		}
	}
	if victim != "" {
		delete(g.store, victim)
	}
}

func (g *IdempotencyGuard) reaper() {
	ticker := time.NewTicker(2 * g.ttlSorrogate()) // NewTicker panics on a zero TTL
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-g.ttlSorrogate())
		g.mu.Lock()
		for k, v := range g.store {
			if v.expires.Before(cutoff) || v.expires.Before(time.Now()) {
				delete(g.store, k)
			}
		}
		g.mu.Unlock()
	}
}

func (g *IdempotencyGuard) ttlSorrogate() time.Duration {
	if g.ttl <= 0 {
		return time.Minute
	}
	return g.ttl
}

// captureWriter records the downstream response.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *captureWriter) WriteHeader(status int)      { c.status = status }
