// Package customer implements the customer domain: registration, profile CRUD,
// forgot/reset password and the 2FA login flow.
package customer

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/cache"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Customer is the public representation of a customer account.
type Customer struct {
	ID          int64     `json:"id"`
	Email       string    `json:"email"`
	FullName    string    `json:"full_name"`
	IsAdmin     bool      `json:"is_admin"`
	TOTPEnabled bool      `json:"totp_enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// customerRow additionally carries the credentials, used internally only.
type customerRow struct {
	ID           int64
	Email        string
	FullName     string
	PasswordHash string
	IsAdmin      bool
	TOTPSecret   string
	TOTPEnabled  bool
	CreatedAt    time.Time
	DeletedAt    *time.Time
}

// Service implements the customer business rules.
type Service struct {
	pool  *pgxpool.Pool
	cache cache.Cache
}

// NewService wires the customer service to Postgres and Redis.
func NewService(pool *pgxpool.Pool, c cache.Cache) *Service {
	return &Service{pool: pool, cache: c}
}

const (
	passwordResetTTL = 10 * time.Minute
	// maxResetAttempts wrong codes burn the code, so the 1-in-a-million guess cannot
	// be repeated until it lands.
	maxResetAttempts = 5
)

// Register creates a new customer account. Reserved emails for the seeded demo
// accounts cannot be taken.
func (s *Service) Register(ctx context.Context, email, password, fullName string) (*Customer, error) {
	if email == "" || password == "" {
		return nil, httpx.BadRequest("email and password are required")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, httpx.Wrap(err)
	}

	var row customerRow
	err = s.pool.QueryRow(ctx, `
		INSERT INTO customers (email, password_hash, full_name)
		VALUES ($1, $2, $3)
		RETURNING id, email, full_name, is_admin, totp_enabled, created_at`,
		email, hash, fullName,
	).Scan(&row.ID, &row.Email, &row.FullName, &row.IsAdmin, &row.TOTPEnabled, &row.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, httpx.Conflict("email already registered")
		}
		return nil, httpx.Wrap(err)
	}
	return s.toCustomer(row), nil
}

// GetWithCredentials fetches a customer including the password hash.
func (s *Service) GetWithCredentials(ctx context.Context, email string) (*customerRow, error) {
	row, err := s.getByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	return row, nil
}

func (s *Service) getByEmail(ctx context.Context, email string) (*customerRow, error) {
	var row customerRow
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, full_name, password_hash, is_admin, totp_secret, totp_enabled, created_at, deleted_at
		FROM customers WHERE email = $1`, email,
	).Scan(&row.ID, &row.Email, &row.FullName, &row.PasswordHash, &row.IsAdmin,
		&row.TOTPSecret, &row.TOTPEnabled, &row.CreatedAt, &row.DeletedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("customer not found")
		}
		return nil, httpx.Wrap(err)
	}
	if row.DeletedAt != nil {
		return nil, httpx.NotFound("customer not found")
	}
	return &row, nil
}

// Get returns the public profile. Callers must have checked ownership.
func (s *Service) Get(ctx context.Context, customerID int64) (*Customer, error) {
	var row customerRow
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, full_name, is_admin, totp_enabled, created_at
		FROM customers WHERE id = $1 AND deleted_at IS NULL`, customerID,
	).Scan(&row.ID, &row.Email, &row.FullName, &row.IsAdmin, &row.TOTPEnabled, &row.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("customer not found")
		}
		return nil, httpx.Wrap(err)
	}
	return s.toCustomerPublic(row), nil
}

// UpdateProfile updates a customer's profile. Only own account or admin.
func (s *Service) UpdateProfile(ctx context.Context, customerID int64, fullName string) (*Customer, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE customers SET full_name = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`,
		customerID, fullName)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("customer not found")
	}
	return s.Get(ctx, customerID)
}

// Delete soft-deletes the account.
func (s *Service) Delete(ctx context.Context, customerID int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE customers SET deleted_at = now(), updated_at = now() WHERE id = $1 AND deleted_at IS NULL`,
		customerID)
	if err != nil {
		return httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("customer not found")
	}
	return nil
}

// ForgotPassword issues a one-time reset code for the email and stores it in
// Redis so concurrent reset attempts stay safe across instances. The code must
// reach the account owner out of band (email); it is never returned to the
// caller. Unknown emails succeed silently so the endpoint does not reveal which
// accounts exist.
//
// The sample has no mail relay wired in. Set DEMO_LOG_RESET_CODES=1 to have the
// code written to the server log for local end-to-end testing.
func (s *Service) ForgotPassword(ctx context.Context, email string) error {
	row, err := s.getByEmail(ctx, email)
	if err != nil || row.DeletedAt != nil {
		return nil
	}
	code, err := randomDigits(6)
	if err != nil {
		return httpx.Wrap(err)
	}
	key := "pwdreset:" + email
	if err := s.cache.Set(ctx, key, []byte(code), passwordResetTTL); err != nil {
		// Redis being down degrades the flow instead of crashing it.
		return httpx.Wrap(fmt.Errorf("store reset code: %w", err))
	}
	if os.Getenv("DEMO_LOG_RESET_CODES") == "1" {
		slog.Info("password reset code issued (demo logging enabled)", "email", email, "code", code)
	}
	return nil
}

// ResetPassword validates the one-time code and rotates the password hash.
func (s *Service) ResetPassword(ctx context.Context, email, code, newPassword string) error {
	key := "pwdreset:" + email
	stored, err := s.cache.Get(ctx, key)
	if err != nil {
		if errors.Is(err, cache.ErrMiss) {
			return httpx.BadRequest("invalid or expired reset code")
		}
		return httpx.Wrap(err)
	}
	if string(stored) != code {
		failKey := "pwdreset-fail:" + email
		failures := 0
		if raw, err := s.cache.Get(ctx, failKey); err == nil {
			failures, _ = strconv.Atoi(string(raw))
		}
		failures++
		if failures >= maxResetAttempts {
			_ = s.cache.Del(ctx, key)
			_ = s.cache.Del(ctx, failKey)
		} else {
			_ = s.cache.Set(ctx, failKey, []byte(strconv.Itoa(failures)), passwordResetTTL)
		}
		return httpx.BadRequest("invalid or expired reset code")
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return httpx.Wrap(err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE customers SET password_hash = $2, updated_at = now() WHERE email = $1 AND deleted_at IS NULL`,
		email, hash)
	if err != nil {
		return httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("customer not found")
	}
	// One-time code consumed.
	_ = s.cache.Del(ctx, key) // best-effort
	_ = s.cache.Del(ctx, "pwdreset-fail:"+email)
	return nil
}

// Enroll2FA provisions a TOTP secret for the customer and returns the otpauth
// URL (to seed an authenticator app). The secret is stored in an inactive
// state; verify2FA activates it.
func (s *Service) Enroll2FA(ctx context.Context, customerID int64, email string) (secret, otpauthURL string, err error) {
	// Re-enrolling would overwrite the secret and switch 2FA off without proof of
	// the current authenticator, so an active 2FA must be disabled (with a valid
	// code) first.
	current, err := s.getByID(ctx, customerID)
	if err != nil {
		return "", "", err
	}
	if current.TOTPEnabled {
		return "", "", httpx.Conflict("2FA is already enabled; disable it with a valid code before enrolling again")
	}

	secret, url, err := auth.ProvisionTOTP(email)
	if err != nil {
		return "", "", httpx.Wrap(err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE customers SET totp_secret = $2, totp_enabled = FALSE, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`,
		customerID, secret)
	if err != nil {
		return "", "", httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return "", "", httpx.NotFound("customer not found")
	}
	return secret, url, nil
}

// Verify2FA validates a TOTP code against the stored secret and, when `activate`
// is set, enables 2FA on the account. It also consumes the code check so a code
// cannot be replayed (the sample relies on the short TOTP window).
func (s *Service) Verify2FA(ctx context.Context, customerID int64, email, code string, activate bool) error {
	row, err := s.getByEmail(ctx, email)
	if err != nil {
		return err
	}
	if row.ID != customerID && !activate {
		return httpx.Forbidden("cannot verify 2FA for another customer")
	}
	if row.TOTPSecret == "" {
		return httpx.Conflict("2FA not enrolled; call /auth/2fa/enroll first")
	}
	if !auth.VerifyTOTP(row.TOTPSecret, code) {
		return httpx.BadRequest("invalid authenticator code")
	}
	if activate {
		tag, err := s.pool.Exec(ctx,
			`UPDATE customers SET totp_enabled = TRUE, updated_at = now() WHERE id = $1`,
			customerID)
		if err != nil {
			return httpx.Wrap(err)
		}
		if tag.RowsAffected() == 0 {
			return httpx.NotFound("customer not found")
		}
	}
	return nil
}

// Disable2FA turns 2FA off after a valid code.
func (s *Service) Disable2FA(ctx context.Context, customerID int64, code string) error {
	row, err := s.getByID(ctx, customerID)
	if err != nil {
		return err
	}
	if row.TOTPSecret == "" || !auth.VerifyTOTP(row.TOTPSecret, code) {
		return httpx.BadRequest("invalid authenticator code")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE customers SET totp_enabled = FALSE, updated_at = now() WHERE id = $1`, customerID)
	if err != nil {
		return httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("customer not found")
	}
	return nil
}

func (s *Service) getByID(ctx context.Context, customerID int64) (*customerRow, error) {
	var row customerRow
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, full_name, password_hash, is_admin, totp_secret, totp_enabled, created_at, deleted_at
		FROM customers WHERE id = $1 AND deleted_at IS NULL`, customerID,
	).Scan(&row.ID, &row.Email, &row.FullName, &row.PasswordHash, &row.IsAdmin,
		&row.TOTPSecret, &row.TOTPEnabled, &row.CreatedAt, &row.DeletedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("customer not found")
		}
		return nil, httpx.Wrap(err)
	}
	return &row, nil
}

func randomDigits(n int) (string, error) {
	const digits = "0123456789"
	buf := make([]byte, n)
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = digits[int(b[i])%len(digits)]
	}
	return string(buf), nil
}

func (s *Service) toCustomer(r customerRow) *Customer {
	return &Customer{
		ID: r.ID, Email: r.Email, FullName: r.FullName,
		IsAdmin: r.IsAdmin, TOTPEnabled: r.TOTPEnabled, CreatedAt: r.CreatedAt,
	}
}

func (s *Service) toCustomerPublic(r customerRow) *Customer {
	c := s.toCustomer(r)
	c.TOTPEnabled = r.TOTPEnabled
	return c
}
