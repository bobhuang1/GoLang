// Package email queues outgoing messages in an outbox table and delivers them
// through an SMTP *relay* (never direct-to-recipient-MX). A pool of parallel
// workers claims pending rows with FOR UPDATE SKIP LOCKED so no two workers
// grab the same row, hands each to the relay seam, and on a transient failure
// retries with exponential backoff. When a message finally cannot be delivered
// (permanent rejection, or attempts exhausted) the row is marked failed and an
// admin_notices row is written so an operator is eventually told.
//
// The relay takes an arbitrary To address: recipient addresses may be external
// customers or internal staff. Only the address differs; the delivery path is
// identical. Failed deliveries always land an internal admin_notices row no
// matter who the recipient was.
//
// The sQLPool seam (narrow, mirrors internal/payment) and the Relay seam let a
// test drive the whole claim/deliver/retry/fail flow with a scripted pgxmock
// pool plus a stub relay, no live DB.
package email

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrTransient marks a relay failure worth retrying (SMTP 4xx, timeouts,
// relay unavailable). Anything else is permanent and terminal.
var ErrTransient = errors.New("email relay: transient failure")

// IsTransient reports whether err should be retried with backoff.
func IsTransient(err error) bool { return errors.Is(err, ErrTransient) }

// Params is what the relay needs to submit one message to SMTP.
type Params struct {
	To             string // external customer or internal staff address
	Subject        string
	Body           string
	IdempotencyKey string
}

// Relay is the SMTP relay seam. A production impl wraps an SMTP relay/SES
// endpoint; tests inject a scripted stub. Transient failures must wrap
// ErrTransient.
type Relay interface {
	Send(ctx context.Context, params Params) error
}

// Status is the outbox row lifecycle.
type Status string

// Outbox row statuses.
const (
	StatusPending   Status = "pending"
	StatusSucceeded Status = "delivered"
	StatusFailed    Status = "failed"
)

// SQL lives as package consts so the worker and the tests reference the exact
// same strings — no whitespace drift means pgxmock expectations always match.
const (
	claimSQL = `SELECT id::text, to_address, subject, body, idempotency_key, attempts
FROM email_messages
WHERE status = 'pending'
  AND (attempts = 0 OR next_attempt_at <= now())
ORDER BY created_at
FOR UPDATE SKIP LOCKED
LIMIT 1`

	bumpSQL = `UPDATE email_messages
SET attempts = attempts + 1,
    next_attempt_at = now() + make_interval(secs => $2::double precision / 1000.0),
    failure_reason = $3,
    updated_at = now()
WHERE id = $1::uuid AND status = 'pending'`

	deliveredSQL = `UPDATE email_messages
SET status = 'delivered', sent_at = now(), updated_at = now()
WHERE id = $1::uuid AND status = 'pending'`

	failSQL = `UPDATE email_messages
SET status = 'failed', failure_reason = $2, updated_at = now()
WHERE id = $1::uuid AND status = 'pending'`

	alertSQL = `INSERT INTO admin_notices (kind, title, body, status)
VALUES ('email_failed', $1, $2, 'new')`
)

// Service owns the outbox and the worker pool.
type Service struct {
	pool       sQLPool
	relay      Relay
	maxAttempt int
	baseDelay  time.Duration
}

// sQLPool is the narrow database seam (mirrors internal/payment).
type sQLPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NewService builds the email outbox worker.
func NewService(pool sQLPool, relay Relay, maxAttempt int, baseDelay time.Duration) *Service {
	return &Service{pool: pool, relay: relay, maxAttempt: maxAttempt, baseDelay: baseDelay}
}

// Run starts N parallel workers that claim and deliver pending emails until ctx
// is cancelled. Each worker owns its claim via SKIP LOCKED so no two workers
// take the same row concurrently.
func (s *Service) Run(ctx context.Context, workers int) error {
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if err := s.processOne(ctx); err != nil && ctx.Err() == nil {
					slog.Error("email worker error", "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// processOne claims a single pending email, delivers it through the relay with
// transient retry/backoff, and marks it delivered or (after maxAttempt)
// failed + admin-noticed. An empty queue is a quiet no-op.
func (s *Service) processOne(ctx context.Context) error {
	var (
		id, to, subject, body, idemKey string
		attempts                       int
	)
	err := s.pool.QueryRow(ctx, claimSQL).Scan(&id, &to, &subject, &body, &idemKey, &attempts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // queue empty
		}
		return httpx.Wrap(err)
	}

	var lastErr error
	for attempt := 1; attempt <= s.maxAttempt; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(backoffDelay(attempt-1, s.baseDelay)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		deliverErr := s.relay.Send(ctx, Params{To: to, Subject: subject, Body: body, IdempotencyKey: idemKey})
		if deliverErr == nil {
			_, e := s.pool.Exec(ctx, deliveredSQL, id)
			if e != nil {
				return httpx.Wrap(e)
			}
			return nil
		}
		lastErr = deliverErr
		if IsTransient(deliverErr) {
			_, e := s.pool.Exec(ctx, bumpSQL, id, backoffDelay(attempt, s.baseDelay)/time.Millisecond, deliverErr.Error())
			if e != nil {
				return httpx.Wrap(e)
			}
			continue
		}
		// Permanent relay rejection: fail now and let an admin know.
		return s.failForAdmin(ctx, id, deliverErr.Error())
	}
	// Attempts exhausted without a delivery: fail and let an admin know.
	return s.failForAdmin(ctx, id, lastErr.Error())
}

// failForAdmin flips the row to failed and records an admin_notices row so an
// operator is eventually told about the undeliverable message.
func (s *Service) failForAdmin(ctx context.Context, id, reason string) error {
	if _, err := s.pool.Exec(ctx, failSQL, id, reason); err != nil {
		return httpx.Wrap(err)
	}
	_, err := s.pool.Exec(ctx, alertSQL, "email delivery failed: "+id, reason)
	return httpx.Wrap(err)
}

// backoffDelay doubles base per attempt after the first, mirroring
// internal/cache. Overflow collapses to baseDelay.
func backoffDelay(attempt int, base time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d <= 0 {
			return base
		}
	}
	return d
}
