package payment

import (
	"context"
	"testing"
	"time"

	"github.com/bobhuang1/GoLang/internal/cache"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"
)

// TestServiceCharge_RestartAfterTransient is a Service-level test that runs
// against a *stub pool* (pgxmock) and the deterministic stub gateway, with the
// provider wired to go transient on first contact ("network") and then settle.
//
// A process restart is modelled exactly as a crash mid-payment: the first
// attempt claims the charge (begin/insert/commit), the provider call fails
// transientlyabb, the charge is left pending with a failure_reason, and the
// Service's own backoff+retry loop restarts the claim. On the restarted
// attempt it finds the already-inserted pending charge by idempotency key,
// resumes it (no second insert), the provider settles on replay, and the
// charge+order are finalised. One Charge() call exercises the whole path.
func TestServiceCharge_RestartAfterTransient(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	const (
		orderID    int64 = 1001
		customerID int64 = 55
		amount     int64 = 25000
		currency         = "usd"
		chargeID         = "ch_restart_1"
	)
	idemKey := "idem-restart-1"

	srv := NewService(pool, cache.Null{}, NewStubGateway(), 3, time.Millisecond)

	// --- attempt 1: fresh claim that hits a transient provider fault ---
	// No prior charge for this idempotency key yet -> fresh claim.
	pool.ExpectQuery(`SELECT id::text.*FROM charges WHERE idempotency_key = \$1 AND customer_id = \$2`).
		WithArgs(idemKey, customerID).
		WillReturnError(pgx.ErrNoRows)

	// Claim transaction: lock the pending order, then insert the charge.
	pool.ExpectBegin()
	pool.ExpectQuery(`SELECT status, total_cents, currency FROM orders WHERE id = \$1 AND customer_id = \$2 FOR UPDATE`).
		WithArgs(orderID, customerID).
		WillReturnRows(pgxmock.NewRows([]string{"status", "total_cents", "currency"}).
			AddRow("pending", amount, currency))
	pool.ExpectQuery(`INSERT INTO charges .* RETURNING id::text`).
		WithArgs(orderID, customerID, amount, currency, idemKey).
		WillReturnRows(pgxmock.NewRows([]string{"id::text"}).AddRow(chargeID))
	pool.ExpectCommit()

	// Provider fails transiently ("network"): mark the claim transient.
	pool.ExpectBegin()
	pool.ExpectExec(`UPDATE charges SET failure_reason = \$2`).
		WithArgs(chargeID, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	pool.ExpectCommit()

	// --- attempt 2 (restart, after backoff): resume the interrupted charge ---
	// The pending charge is now visible by idempotency key.
	pool.ExpectQuery(`SELECT id::text.*FROM charges WHERE idempotency_key = \$1 AND customer_id = \$2`).
		WithArgs(idemKey, customerID).
		WillReturnRows(pgxmock.NewRows([]string{"id::text", "order_id", "customer_id",
			"amount_cents", "currency", "status", "provider_charge_id", "failure_reason",
			"idempotency_key", "created_at"}).
			AddRow(chargeID, orderID, customerID, amount, currency, string(ChargePending),
				"", "stub: upstream network timeout", idemKey, time.Now()))

	// Provider settles on replay -> finalise charge + order together.
	pool.ExpectBegin()
	pool.ExpectExec(`UPDATE charges SET status = 'succeeded', provider_charge_id = \$2`).
		WithArgs(chargeID, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	pool.ExpectExec(`UPDATE orders SET status = 'paid'`).
		WithArgs(orderID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	pool.ExpectCommit()

	charge, err := srv.Charge(ctx, orderID, customerID, idemKey, SimulateNetwork)
	if err != nil {
		t.Fatalf("Charge: unexpected error: %v", err)
	}
	if charge == nil {
		t.Fatal("Charge returned nil")
	}
	if charge.Status != ChargeSucceeded {
		t.Fatalf("expected status %q, got %q", ChargeSucceeded, charge.Status)
	}
	if charge.ID != chargeID {
		t.Fatalf("expected charge id %q, got %q", chargeID, charge.ID)
	}

	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("stub pool expectations not met: %v", err)
	}
}
