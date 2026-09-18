package email

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/pashagolub/pgxmock/v5"
)

const id = "7f3c0d8e-1a2b-4c3d-8e4f-5a6b7c8d9e0f"

// stubRelay records calls and replays a fixed script of outcomes. A nil entry
// delivers; an error entry fails that attempt (transient or permanent).
type stubRelay struct {
	outcomes []error
	got      []Params
}

func (r *stubRelay) Send(_ context.Context, p Params) error {
	r.got = append(r.got, p)
	if len(r.outcomes) == 0 {
		return nil
	}
	err := r.outcomes[0]
	r.outcomes = r.outcomes[1:]
	return err
}

// mainPayload is what processOne passes to the relay for the shared row.
func mainPayload() Params {
	return Params{To: "admin@internal.example", Subject: "Hello", Body: "Body", IdempotencyKey: "idem-1"}
}

// expectClaim wires the claim query to return the single pending row.
func expectClaim(pool pgxmock.PgxPoolIface) {
	pool.ExpectQuery(claimSQL).WillReturnRows(
		pgxmock.NewRows([]string{"id", "to_address", "subject", "body", "idempotency_key", "attempts"}).
			AddRow(id, mainPayload().To, mainPayload().Subject, mainPayload().Body, mainPayload().IdempotencyKey, 0),
	)
}

// TestProcessOneTransientThenDelivered covers the transient path: the first
// relay attempt hits a transient failure (immediately bumping attempts + backoff
// so the row stays claimable with a future next_attempt_at), and the retry
// delivers.
func TestProcessOneTransientThenDelivered(t *testing.T) {
	pool, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer pool.Close()

	relay := &stubRelay{outcomes: []error{ErrTransient, nil}}
	svc := NewService(pool, relay, 3, 100*time.Millisecond)

	expectClaim(pool)
	// attempt 1 transient -> bump attempts + backoff + reason
	pool.ExpectExec(bumpSQL).
		WithArgs(id, time.Duration(100), ErrTransient.Error()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// attempt 2 delivered
	pool.ExpectExec(deliveredSQL).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := svc.processOne(context.Background()); err != nil {
		var apiErr *httpx.APIError
		if errors.As(err, &apiErr) && apiErr.Err != nil {
			t.Fatalf("processOne: %v (cause: %+v)", apiErr, apiErr.Err)
			return
		}
		t.Fatalf("processOne: %v", err)
		return
	}

	if got := len(relay.got); got != 2 {
		t.Fatalf("expected 2 relay calls, got %d", got)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet DB expectations: %v", err)
	}
}

// TestProcessOnePermanentFailsAndAlertsAdmin covers the permanent path: a
// non-transient relay error immediately fails the row AND records an alert so
// an operator is told.
func TestProcessOnePermanentFailsAndAlertsAdmin(t *testing.T) {
	pool, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer pool.Close()

	permErr := errors.New("relay rejected: 550 permanently undeliverable")
	relay := &stubRelay{outcomes: []error{permErr}}
	svc := NewService(pool, relay, 3, 100*time.Millisecond)

	expectClaim(pool)
	pool.ExpectExec(failSQL).
		WithArgs(id, permErr.Error()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	pool.ExpectExec(alertSQL).
		WithArgs("email delivery failed: "+id, permErr.Error()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := svc.processOne(context.Background()); err != nil {
		var apiErr *httpx.APIError
		if errors.As(err, &apiErr) && apiErr.Err != nil {
			t.Fatalf("processOne: %v (cause: %+v)", apiErr, apiErr.Err)
			return
		}
		t.Fatalf("processOne: %v", err)
		return
	}

	if got := len(relay.got); got != 1 {
		t.Fatalf("expected 1 relay call, got %d", got)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet DB expectations: %v", err)
	}
}

// TestBackoffDelay pins the exponential ladder: attempt 1 = base, attempt 2 =
// 2x, attempt 3 = 4x, attempt 4 = 8x.
func TestBackoffDelay(t *testing.T) {
	base := 100 * time.Millisecond
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, base},
		{2, 2 * base},
		{3, 4 * base},
		{4, 8 * base},
	}
	for _, c := range cases {
		if got := backoffDelay(c.attempt, base); got != c.want {
			t.Fatalf("backoffDelay(%d): got %s want %s", c.attempt, got, c.want)
		}
	}
}
