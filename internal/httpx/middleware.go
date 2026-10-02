package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/go-chi/chi/v5/middleware"
)

// RequestIDMW injects a request id into the context and headers.
func RequestIDMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RecoverMW converts panics into 500s and logs the stack.
func RecoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered",
					"request_id", RequestID(r.Context()),
					"path", r.URL.Path,
					"panic", fmt.Sprint(rec),
					"stack", string(debug.Stack()))
				WriteJSON(w, http.StatusInternalServerError, Envelope{
					Error: EnvelopeError{Code: "internal", Message: "internal error"},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// LoggerMW logs each request with duration.
func LoggerMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("http request",
			"request_id", RequestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds())
	})
}

// RequireAuth verifies the Bearer token and stores the caller in context.
func RequireAuth(tokens *auth.TokenManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearer(r)
			if !ok {
				WriteError(w, Unauthorized("missing bearer token"))
				return
			}
			claims, err := tokens.Parse(token)
			if err != nil {
				WriteError(w, Unauthorized("invalid or expired token"))
				return
			}
			// Only a full access token authenticates a request. The 2FA challenge
			// token proves the password step alone and is redeemable solely at
			// /auth/login/2fa, which parses it itself.
			if claims.Kind != auth.TokenKindFull {
				WriteError(w, Unauthorized("invalid or expired token"))
				return
			}
			ctx := WithUser(r.Context(), &ContextUser{
				CustomerID: parseSubject(claims.Subject),
				Email:      claims.Email,
				Role:       claims.Role,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdmin restricts a route to authenticated admins.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFrom(r.Context())
		if user == nil {
			WriteError(w, Unauthorized("authentication required"))
			return
		}
		if user.Role != auth.RoleAdmin {
			WriteError(w, Forbidden("administrator role required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) (string, bool) {
	raw := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(raw) < len(prefix) || raw[:len(prefix)] != prefix {
		return "", false
	}
	return raw[len(prefix):], true
}

func parseSubject(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
