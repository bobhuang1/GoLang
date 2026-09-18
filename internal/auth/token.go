// Package auth implements JWT access tokens, bcrypt passwords and TOTP 2FA.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Roles understood by the authorization middleware.
const (
	RoleCustomer = "customer"
	RoleAdmin    = "admin"
	// TokenKindChallenge marks the short-lived token handed out right after a
	// successful password login when 2FA is enabled. It can only be redeemed
	// by the /auth/2fa/verify endpoint.
	TokenKindFull      = "full"
	TokenKindChallenge = "2fa_challenge"
)

// Claims is the JWT payload used across the API.
type Claims struct {
	Email string `json:"email"`
	Role  string `json:"role"`
	Kind  string `json:"kind"`
	jwt.RegisteredClaims
}

// TokenManager signs and verifies HS256 JWTs.
type TokenManager struct {
	secret       []byte
	ttl          time.Duration
	challengeTTL time.Duration
}

// NewTokenManager builds a TokenManager for the given secret and TTLs.
func NewTokenManager(secret string, ttl, challengeTTL time.Duration) *TokenManager {
	return &TokenManager{secret: []byte(secret), ttl: ttl, challengeTTL: challengeTTL}
}

// Sign issues a full access token for the customer.
func (tm *TokenManager) Sign(customerID int64, email, role string) (string, error) {
	return tm.sign(customerID, email, role, TokenKindFull, tm.ttl)
}

// SignChallenge issues a short-lived token that proves the password step of a
// 2FA login. It carries the same subject but can only be used with the 2FA
// redemption endpoint.
func (tm *TokenManager) SignChallenge(customerID int64, email, role string) (string, error) {
	return tm.sign(customerID, email, role, TokenKindChallenge, tm.challengeTTL)
}

func (tm *TokenManager) sign(customerID int64, email, role, kind string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		Email: email,
		Role:  role,
		Kind:  kind,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprint(customerID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "go-shop",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(tm.secret)
}

// Parse validates a token and returns its claims.
func (tm *TokenManager) Parse(raw string) (*Claims, error) {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return tm.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

// ChallengeKind reports whether the claims are a short-lived 2FA challenge.
func ChallengeKind(claims *Claims) bool { return claims.Kind == TokenKindChallenge }
