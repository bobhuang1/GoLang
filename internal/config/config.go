// Package config loads the server configuration from environment variables.
package config

import (
	"os"
	"time"
)

// DefaultJWTSecret is the development fallback for JWT_SECRET. The server refuses to
// start with it outside demo mode.
const DefaultJWTSecret = "dev-secret-change-me"

// Config holds every runtime knob of the server.
type Config struct {
	HTTPAddr              string
	DatabaseURL           string
	RedisAddr             string
	RedisPassword         string
	JWTSecret             string
	JWTTTL                time.Duration
	JWTChallengeTTL       time.Duration
	MaxPaymentAttempts    int
	PaymentRetryBaseDelay time.Duration
}

// FromEnv reads configuration from environment variables, applying defaults.
func FromEnv() Config {
	return Config{
		HTTPAddr:              getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:           getenv("DATABASE_URL", "postgres://shop:shop@localhost:5432/shop?sslmode=disable"),
		RedisAddr:             getenv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:         os.Getenv("REDIS_PASSWORD"),
		JWTSecret:             getenv("JWT_SECRET", DefaultJWTSecret),
		JWTTTL:                time.Duration(getenvInt("JWT_TTL_MINUTES", 60)) * time.Minute,
		JWTChallengeTTL:       time.Duration(getenvInt("JWT_CHALLENGE_TTL_MINUTES", 5)) * time.Minute,
		MaxPaymentAttempts:    getenvInt("MAX_PAYMENT_ATTEMPTS", 4),
		PaymentRetryBaseDelay: time.Duration(getenvInt("PAYMENT_RETRY_BASE_DELAY_MS", 100)) * time.Millisecond,
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	v, ok := parseDecimal(os.Getenv(key))
	if !ok {
		return fallback
	}
	return v
}

func parseDecimal(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
