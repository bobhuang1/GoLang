package httpx

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdempotencyGuard(t *testing.T) {
	var calls atomic.Int32
	status := http.StatusCreated
	handler := NewIdempotencyGuard(time.Minute, 100).Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(r.Header.Get("Authorization")))
	}))

	do := func(method, auth, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/payments/charges", nil)
		req.Header.Set("Authorization", auth)
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	first := do(http.MethodPost, "Bearer alice", "k1")
	replay := do(http.MethodPost, "Bearer alice", "k1")
	if calls.Load() != 1 || replay.Header().Get("X-Idempotent-Replay") != "true" || replay.Body.String() != first.Body.String() {
		t.Fatalf("same caller + key should replay: calls=%d", calls.Load())
	}

	other := do(http.MethodPost, "Bearer bob", "k1")
	if calls.Load() != 2 || other.Body.String() != "Bearer bob" {
		t.Fatalf("another caller reusing the key must not see the first caller's response: %q", other.Body.String())
	}

	status = http.StatusInternalServerError
	do(http.MethodPost, "Bearer alice", "k2")
	do(http.MethodPost, "Bearer alice", "k2")
	if calls.Load() != 4 {
		t.Fatalf("failed responses must not be cached: calls=%d", calls.Load())
	}

	status = http.StatusOK
	do(http.MethodGet, "Bearer alice", "k3")
	do(http.MethodGet, "Bearer alice", "k3")
	if calls.Load() != 6 {
		t.Fatalf("GET requests must not be replayed: calls=%d", calls.Load())
	}
}
