package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bobhuang1/GoLang/internal/cache"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// sQLPool is the narrow database seam Service depends on. Both the real
// *pgxpool.Pool and pgxmock's stub pool satisfy it, so the payment process can
// be exercised at the service level (including the restart-after-transient
// path) with an in-memory pool and no live database.
type sQLPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Service orchestrates charges and refunds. Payment is split into a short DB
// transaction, then an external (stub) provider call, then a finalisation
// transaction. Because every write is keyed by an idempotency key with a
// UNIQUE constraint, any step can be restarted safely.
type Service struct {
	pool       sQLPool
	cache      cache.Cache
	gateway    Gateway
	maxAttempt int
	baseDelay  time.Duration
}

// NewService wires payment to its dependencies.
func NewService(pool sQLPool, c cache.Cache, gw Gateway, maxAttempt int, baseDelay time.Duration) *Service {
	return &Service{pool: pool, cache: c, gateway: gw, maxAttempt: maxAttempt, baseDelay: baseDelay}
}

// Charge is the persisted, provider-backed charge.
type Charge struct {
	ID             string       `json:"id"`
	OrderID        int64        `json:"order_id"`
	CustomerID     int64        `json:"customer_id"`
	AmountCents    int64        `json:"amount_cents"`
	Currency       string       `json:"currency"`
	Status         ChargeStatus `json:"status"`
	ProviderCharge string       `json:"provider_charge_id,omitempty"`
	FailureReason  string       `json:"failure_reason,omitempty"`
	IdempotencyKey string       `json:"idempotency_key"`
	CreatedAt      time.Time    `json:"created_at"`
}

// Refund is a persisted, provider-backed refund.
type Refund struct {
	ID             string       `json:"id"`
	ChargeID       string       `json:"charge_id"`
	OrderID        int64        `json:"order_id"`
	CustomerID     int64        `json:"customer_id"`
	AmountCents    int64        `json:"amount_cents"`
	Status         RefundStatus `json:"status"`
	ProviderRefund string       `json:"provider_refund_id,omitempty"`
	FailureReason  string       `json:"failure_reason,omitempty"`
	IdempotencyKey string       `json:"idempotency_key"`
	CreatedAt      time.Time    `json:"created_at"`
}

func sessionKey(kind, key string) string { return "payment:session:" + kind + ":" + key }

// Charge takes payment for an order. The flow is:
//
//  1. Write a Redis session marker first. If the cache is down this is treated
//     as a transient condition that triggers a restart of the payment process
//     (the requirement "if redis caching fails, restart the payment process").
//  2. Claim the charge inside a transaction (INSERT guarded by the UNIQUE
//     idempotency key + FOR UPDATE order lock). Rollback on any failure.
//  3. Call the provider. Transient failures restart; permanent declines fail
//     the charge; successes are finalised in a second transaction.
//
// The whole routine is idempotent: a retry with the same key replays either the
// stored charge or, in the failure window, safely creates exactly one.
func (s *Service) Charge(ctx context.Context, orderID, customerID int64, idemKey, simulate string) (*Charge, error) {
	var lastErr error
	for attempt := 1; attempt <= s.maxAttempt; attempt++ {
		if attempt > 1 {
			delay := cache.BackoffDelay(attempt-1, s.baseDelay)
			slog.Warn("payment process restarting",
				"order_id", orderID, "idem_key", idemKey, "attempt", attempt-1, "delay_ms", delay.Milliseconds(), "err", lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		charge, err := s.chargeOnce(ctx, orderID, customerID, idemKey, simulate)
		if err == nil {
			return charge, nil
		}
		if IsTransient(err) {
			lastErr = err
			continue
		}
		return nil, err
	}
	return nil, httpx.Internal("payment failed after retries", lastErr)
}

func (s *Service) chargeOnce(ctx context.Context, orderID, customerID int64, idemKey, simulate string) (*Charge, error) {
	// Redis session marker. Its failure restarts the whole payment process.
	marker := []byte(fmt.Sprintf(`{"order_id":%d,"key":%q}`, orderID, idemKey))
	if err := s.cache.Set(ctx, sessionKey("charge", idemKey), marker, 15*time.Minute); err != nil {
		return nil, NewTransient(fmt.Errorf("redis caching failed: %w", err))
	}

	// Replay / claim the charge. A finalised outcome is replayed as-is; a
	// claimed-but-unsettled charge (pending, no provider ref) means the payment
	// process was interrupted mid-flight after a transient failure — resume it
	// below by re-invoking the provider under the same idempotency key instead
	// of replaying a stale pending row to the caller.
	replayed, err := s.findChargeByKey(ctx, idemKey)
	if err != nil {
		return nil, err
	}
	if replayed != nil && (replayed.Status != ChargePending || replayed.ProviderCharge != "") {
		return replayed, nil
	}

	// Fresh claim or resume of an interrupted (pending, no provider ref) charge.
	// Reused amounts/currency only apply on the resume path; a fresh claim loads
	// its own values from the row lock below.
	chargeID := ""
	var amount int64
	var currency string
	if replayed != nil {
		chargeID = replayed.ID
		amount = replayed.AmountCents
		currency = replayed.Currency
	}
	if replayed == nil {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return nil, httpx.Wrap(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		var oStatus string
		if err := tx.QueryRow(ctx, `
			SELECT status, total_cents, currency FROM orders
			WHERE id = $1 FOR UPDATE`, orderID,
		).Scan(&oStatus, &amount, &currency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, httpx.NotFound("order not found")
			}
			return nil, httpx.Wrap(err)
		}
		switch oStatus {
		case string(ChargeSucceeded), "refunded", "cancelled", "processing", "shipped", "delivered":
			return nil, httpx.Conflict(fmt.Sprintf("order already has a terminal payment state (%s)", oStatus))
		case "pending":
			// ok, proceed
		default:
			return nil, httpx.Conflict(fmt.Sprintf("order cannot be charged in status %s", oStatus))
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO charges (order_id, customer_id, amount_cents, currency, idempotency_key)
			VALUES ($1,$2,$3,$4,$5)
			RETURNING id::text`,
			orderID, customerID, amount, currency, idemKey,
		).Scan(&chargeID); err != nil {
			return nil, httpx.Wrap(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, httpx.Wrap(err)
		}
	} else {
		chargeID = replayed.ID
	}

	// External provider call (out of the DB transaction).
	meta := map[string]string{}
	if simulate != "" {
		meta["simulate"] = simulate
	}
	res, err := s.gateway.CreateCharge(ctx, ChargeParams{
		Amount:         amount,
		Currency:       currency,
		Description:    fmt.Sprintf("order %d", orderID),
		IdempotencyKey: idemKey,
		Metadata:       meta,
	})
	if err != nil {
		if IsTransient(err) {
			_ = s.markChargeTransient(ctx, chargeID, err.Error())
			return nil, NewTransient(err)
		}
		var perm *PermanentError
		if errors.As(err, &perm) {
			if ferr := s.markChargeFailed(ctx, chargeID, perm.Error()); ferr != nil {
				return nil, httpx.Wrap(ferr)
			}
			return nil, &httpx.APIError{Status: http.StatusUnprocessableEntity,
				Code: "payment_declined", Message: perm.Reason}
		}
		return nil, httpx.Wrap(err)
	}

	charge := &Charge{
		ID: chargeID, OrderID: orderID, CustomerID: customerID,
		AmountCents: amount, Currency: currency, Status: ChargeStatus(res.Status),
		ProviderCharge: res.ID, IdempotencyKey: idemKey, CreatedAt: time.Now().UTC(),
	}

	if res.Status == ChargePending {
		// Async settlement: leave the order pending, record the provider ref.
		if err := s.markChargeProvider(ctx, chargeID, res.ID, string(ChargePending)); err != nil {
			return nil, httpx.Wrap(err)
		}
		return charge, nil
	}
	if res.Status == ChargeFailed {
		if err := s.markChargeFailed(ctx, chargeID, "provider reported failure"); err != nil {
			return nil, httpx.Wrap(err)
		}
		return nil, &httpx.APIError{Status: http.StatusUnprocessableEntity,
			Code: "payment_declined", Message: "provider reported failure"}
	}

	// Success: finalise charge AND flip the order to paid in one transaction.
	if err := s.markPaid(ctx, chargeID, orderID, res.ID); err != nil {
		return nil, httpx.Wrap(err)
	}
	charge.Status = ChargeSucceeded
	_ = s.cache.Del(ctx, sessionKey("charge", idemKey))
	return charge, nil
}

// markPaid finalises a successful charge and moves the order to paid
// atomically. Rolled back entirely if either update fails.
func (s *Service) markPaid(ctx context.Context, chargeID string, orderID int64, providerID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE charges SET status = 'succeeded', provider_charge_id = $2, updated_at = now()
		WHERE id = $1::uuid AND status = 'pending'`, chargeID, providerID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE orders SET status = 'paid', version = version + 1, updated_at = now()
		WHERE id = $1 AND status = 'pending'`, orderID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("order is not pending anymore")
	}
	return tx.Commit(ctx)
}

func (s *Service) markChargeProvider(ctx context.Context, chargeID, providerID, status string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE charges SET status = $2::charge_status, provider_charge_id = $3, updated_at = now()
		WHERE id = $1::uuid`, chargeID, status, providerID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) markChargeFailed(ctx context.Context, chargeID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE charges SET status = 'failed', failure_reason = $2, updated_at = now()
		WHERE id = $1::uuid`, chargeID, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) markChargeTransient(ctx context.Context, chargeID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		UPDATE charges SET failure_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND status = 'pending'`, chargeID, reason)
	if err == nil {
		err = tx.Commit(ctx)
	}
	return err
}

// findChargeByKey replays a previously claimed charge, if the key is known.
func (s *Service) findChargeByKey(ctx context.Context, idemKey string) (*Charge, error) {
	var c Charge
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, order_id, customer_id, amount_cents, currency, status, provider_charge_id,
		       failure_reason, idempotency_key, created_at
		FROM charges WHERE idempotency_key = $1`, idemKey,
	).Scan(&c.ID, &c.OrderID, &c.CustomerID, &c.AmountCents, &c.Currency, &c.Status,
		&c.ProviderCharge, &c.FailureReason, &c.IdempotencyKey, &c.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, httpx.Wrap(err)
	}
	return &c, nil
}

// ChargeByID returns a charge with caller authz (own or admin).
func (s *Service) ChargeByID(ctx context.Context, chargeID string, customerID int64, admin bool) (*Charge, error) {
	c, err := s.chargeByID(ctx, chargeID)
	if err != nil {
		return nil, err
	}
	if !admin && c.CustomerID != customerID {
		return nil, httpx.Forbidden("not allowed to view this charge")
	}
	return c, nil
}

func (s *Service) chargeByID(ctx context.Context, chargeID string) (*Charge, error) {
	var c Charge
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, order_id, customer_id, amount_cents, currency, status, provider_charge_id,
		       failure_reason, idempotency_key, created_at
		FROM charges WHERE id = $1::uuid`, chargeID,
	).Scan(&c.ID, &c.OrderID, &c.CustomerID, &c.AmountCents, &c.Currency, &c.Status,
		&c.ProviderCharge, &c.FailureReason, &c.IdempotencyKey, &c.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("charge not found")
		}
		return nil, httpx.Wrap(err)
	}
	return &c, nil
}

// MyCharges lists the caller's charges.
func (s *Service) MyCharges(ctx context.Context, customerID int64) ([]*Charge, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, order_id, customer_id, amount_cents, currency, status, provider_charge_id,
		       failure_reason, idempotency_key, created_at
		FROM charges WHERE customer_id = $1 ORDER BY created_at DESC`, customerID)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer rows.Close()
	var out []*Charge
	for rows.Next() {
		var c Charge
		if err := rows.Scan(&c.ID, &c.OrderID, &c.CustomerID, &c.AmountCents, &c.Currency, &c.Status,
			&c.ProviderCharge, &c.FailureReason, &c.IdempotencyKey, &c.CreatedAt); err != nil {
			return nil, httpx.Wrap(err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// MyRefunds lists the caller's refunds (with their order ids).
func (s *Service) MyRefunds(ctx context.Context, customerID int64) ([]*Refund, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id::text, r.charge_id::text, c.order_id, r.customer_id, r.amount_cents, r.status,
		       r.provider_refund_id, r.failure_reason, r.idempotency_key, r.created_at
		FROM refunds r JOIN charges c ON c.id = r.charge_id
		WHERE r.customer_id = $1 ORDER BY r.created_at DESC`, customerID)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer rows.Close()
	var out []*Refund
	for rows.Next() {
		var r Refund
		if err := rows.Scan(&r.ID, &r.ChargeID, &r.OrderID, &r.CustomerID, &r.AmountCents, &r.Status,
			&r.ProviderRefund, &r.FailureReason, &r.IdempotencyKey, &r.CreatedAt); err != nil {
			return nil, httpx.Wrap(err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// Refund reverses part or all of a charge. Same idempotency discipline as
// Charge: a UNIQUE key claim, provider call and finalisation transaction.
func (s *Service) Refund(ctx context.Context, orderID, actingCustomer int64, amountCents int64, idemKey, reason string, admin bool) (*Refund, error) {
	charge, err := s.chargeByOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if !admin && charge.CustomerID != actingCustomer {
		return nil, httpx.Forbidden("not allowed to refund this order")
	}
	if charge.Status == ChargeFailed || charge.Status == ChargePending {
		return nil, httpx.Conflict("charge has no collected funds to refund")
	}
	refundedSoFar, err := s.refundedTotal(ctx, charge.ID)
	if err != nil {
		return nil, err
	}
	remaining := charge.AmountCents - refundedSoFar
	if remaining <= 0 {
		return nil, httpx.Conflict("charge is already fully refunded")
	}
	// amount_cents is optional: 0 (or omitted) means "refund everything left".
	if amountCents <= 0 {
		amountCents = remaining
	}
	if amountCents > remaining {
		return nil, httpx.BadRequest(fmt.Sprintf("amount must be between 1 and %d cents", remaining))
	}

	var lastErr error
	for attempt := 1; attempt <= s.maxAttempt; attempt++ {
		if attempt > 1 {
			delay := cache.BackoffDelay(attempt-1, s.baseDelay)
			slog.Warn("refund process restarting",
				"charge_id", charge.ID, "idem_key", idemKey, "attempt", attempt-1, "err", lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		r, err := s.refundOnce(ctx, charge, actingCustomer, amountCents, idemKey, reason)
		if err == nil {
			return r, nil
		}
		if IsTransient(err) {
			lastErr = err
			continue
		}
		return nil, err
	}
	return nil, httpx.Internal("refund failed after retries", lastErr)
}

func (s *Service) refundOnce(ctx context.Context, charge *Charge, actingCustomer int64, amountCents int64, idemKey, reason string) (*Refund, error) {
	if cached, err := s.findRefundByKey(ctx, idemKey); err != nil {
		return nil, err
	} else if cached != nil {
		return cached, nil
	}

	var refundID string
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `
		INSERT INTO refunds (charge_id, customer_id, amount_cents, idempotency_key)
		VALUES ($1::uuid, $2, $3, $4)
		RETURNING id::text`,
		charge.ID, actingCustomer, amountCents, idemKey,
	).Scan(&refundID); err != nil {
		return nil, httpx.Wrap(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, httpx.Wrap(err)
	}

	res, err := s.gateway.RefundCharge(ctx, RefundParams{
		ChargeID:       charge.ProviderCharge,
		Amount:         amountCents,
		IdempotencyKey: idemKey,
		Reason:         reason,
	})
	if err != nil {
		if IsTransient(err) {
			if serr := s.setRefundTransient(ctx, refundID, err.Error()); serr != nil {
				return nil, httpx.Wrap(serr)
			}
			return nil, NewTransient(err)
		}
		if serr := s.setRefundFailed(ctx, refundID, err.Error()); serr != nil {
			return nil, httpx.Wrap(serr)
		}
		return nil, httpx.Wrap(err)
	}

	// Finalise: mark refund delivered and roll the charge to 'refunded' when
	// it is now fully refunded. Atomic, rolled back on any failure.
	if err := s.finaliseRefund(ctx, refundID, charge.ID, res.ID); err != nil {
		return nil, httpx.Wrap(err)
	}
	r := &Refund{
		ID: refundID, ChargeID: charge.ID, OrderID: charge.OrderID,
		CustomerID: charge.CustomerID, AmountCents: amountCents, Status: RefundSucceeded,
		ProviderRefund: res.ID, IdempotencyKey: idemKey, CreatedAt: time.Now().UTC(),
	}
	_ = s.cache.Del(ctx, sessionKey("refund", idemKey))
	return r, nil
}

// finaliseRefund commits the successful refund and marks the charge refunded
// when it is fully covered. Transactional: any failure rolls back both.
func (s *Service) finaliseRefund(ctx context.Context, refundID, chargeID, providerRefundID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE refunds SET status = 'succeeded', provider_refund_id = $2, updated_at = now()
		WHERE id = $1::uuid`, refundID, providerRefundID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE charges SET
			status = CASE WHEN (
				COALESCE((SELECT SUM(amount_cents) FILTER (WHERE status = 'succeeded') FROM refunds WHERE charge_id = $1::uuid), 0)
				>= amount_cents
			) THEN 'refunded' ELSE status END,
			updated_at = now()
		WHERE id = $1::uuid`, chargeID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) setRefundTransient(ctx context.Context, refundID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE refunds SET failure_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND status = 'pending'`, refundID, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) setRefundFailed(ctx context.Context, refundID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE refunds SET status = 'failed', failure_reason = $2, updated_at = now()
		WHERE id = $1::uuid`, refundID, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) chargeByOrder(ctx context.Context, orderID int64) (*Charge, error) {
	var c Charge
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, order_id, customer_id, amount_cents, currency, status, provider_charge_id,
		       failure_reason, idempotency_key, created_at
		FROM charges WHERE order_id = $1`, orderID,
	).Scan(&c.ID, &c.OrderID, &c.CustomerID, &c.AmountCents, &c.Currency, &c.Status,
		&c.ProviderCharge, &c.FailureReason, &c.IdempotencyKey, &c.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("no charge found for this order")
		}
		return nil, httpx.Wrap(err)
	}
	return &c, nil
}

func (s *Service) refundedTotal(ctx context.Context, chargeID string) (int64, error) {
	var total int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_cents) FILTER (WHERE status = 'succeeded'), 0)
		FROM refunds WHERE charge_id = $1::uuid`, chargeID,
	).Scan(&total)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, httpx.Wrap(err)
	}
	return total, nil
}

func (s *Service) findRefundByKey(ctx context.Context, idemKey string) (*Refund, error) {
	var r Refund
	err := s.pool.QueryRow(ctx, `
		SELECT r.id::text, r.charge_id::text, c.order_id, r.customer_id, r.amount_cents, r.status,
		       r.provider_refund_id, r.failure_reason, r.idempotency_key, r.created_at
		FROM refunds r JOIN charges c ON c.id = r.charge_id
		WHERE r.idempotency_key = $1`, idemKey,
	).Scan(&r.ID, &r.ChargeID, &r.OrderID, &r.CustomerID, &r.AmountCents, &r.Status,
		&r.ProviderRefund, &r.FailureReason, &r.IdempotencyKey, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, httpx.Wrap(err)
	}
	return &r, nil
}
