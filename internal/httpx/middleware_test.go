package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bobhuang1/GoLang/internal/auth"
)

func TestRequireAuthAcceptsOnlyFullTokens(t *testing.T) {
	tokens := auth.NewTokenManager("test-secret", time.Hour, 5*time.Minute)

	full, err := tokens.Sign(7, "a@example.test", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := tokens.SignChallenge(7, "a@example.test", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		token  string
		status int
	}{
		{"full token", full, http.StatusOK},
		{"2FA challenge token", challenge, http.StatusUnauthorized},
		{"garbage", "not-a-jwt", http.StatusUnauthorized},
	}

	handler := RequireAuth(tokens)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if UserFrom(r.Context()) == nil {
			t.Error("user missing from context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}
