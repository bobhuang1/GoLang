// Package payment implements idempotent, transactionally-safe charges and
// refunds against a Stripe-shaped gateway. The gateway lives behind an
// interface so a real SDK can be dropped in; a deterministic stub simulates
// success, decline and transient network failures (to demo the payment
// restart + retry path).
package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// ChargeStatus mirrors the DB charge_status enum.
type ChargeStatus string

// Charge statuses.
const (
	ChargePending   ChargeStatus = "pending"
	ChargeSucceeded ChargeStatus = "succeeded"
	ChargeFailed    ChargeStatus = "failed"
	ChargeRefunded  ChargeStatus = "refunded"
)

// RefundStatus mirrors the DB refund_status enum.
type RefundStatus string

// Refund statuses.
const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

// Simulation modes understood by the stub gateway. Pass them through the
// charge/refund request bodies to drive deterministic demo scenarios.
const (
	SimulateNetwork = "network" // first call returns a transient error
	SimulateDecline = "decline" // permanently declined
	SimulateHold    = "hold"    // stays pending (async settlement)
)

// ChargeParams mirrors a stripe.ChargeParams-ish request.
type ChargeParams struct {
	Amount         int64
	Currency       string
	Description    string
	IdempotencyKey string
	Metadata       map[string]string
}

// ChargeResult is how the gateway reports a charge.
type ChargeResult struct {
	ID       string
	Status   ChargeStatus
	Amount   int64
	Currency string
	Last4    string
}

// RefundParams mirrors a stripe.RefundParams-ish request.
type RefundParams struct {
	ChargeID       string
	Amount         int64
	IdempotencyKey string
	Reason         string
}

// RefundResult is how the gateway reports a refund.
type RefundResult struct {
	ID     string
	Status RefundStatus
}

// Gateway is the stripe-shaped payment provider interface.
type Gateway interface {
	CreateCharge(ctx context.Context, params ChargeParams) (*ChargeResult, error)
	GetCharge(ctx context.Context, providerChargeID string) (*ChargeResult, error)
	RefundCharge(ctx context.Context, params RefundParams) (*RefundResult, error)
	GetRefund(ctx context.Context, providerRefundID string) (*RefundResult, error)
}

// TransientError reports a gateway/cache failure that a restart may fix
// (network blip, Redis outage). The payment service retries these.
type TransientError struct{ Cause error }

func (e *TransientError) Error() string { return "transient payment failure: " + e.Cause.Error() }
func (e *TransientError) Unwrap() error { return e.Cause }

// PermanentError reports a definitive card/business rejection (decline).
type PermanentError struct{ Reason string }

func (e *PermanentError) Error() string { return "payment declined: " + e.Reason }

// NewTransient wraps an error as transient.
func NewTransient(err error) error { return &TransientError{Cause: err} }

// IsTransient reports whether err is a transient failure.
func IsTransient(err error) bool {
	var e *TransientError
	return errors.As(err, &e)
}

// --- Stub gateway -----------------------------------------------------------

// rounded stub behaviour: fail the first network call, then settle normally.
type stubGateway struct {
	mu          sync.Mutex
	charges     map[string]*ChargeResult // keyed by provider id
	refunds     map[string]*RefundResult
	byIdemKey   map[string]string // idempotency key -> provider id
	refundByKey map[string]string
	netFirst    map[string]bool // idempotency keys that already "failed" once
	nextID      int
}

// NewStubGateway builds the deterministic stub provider.
func NewStubGateway() Gateway {
	return &stubGateway{
		charges:     map[string]*ChargeResult{},
		refunds:     map[string]*RefundResult{},
		byIdemKey:   map[string]string{},
		refundByKey: map[string]string{},
		netFirst:    map[string]bool{},
	}
}

func (g *stubGateway) CreateCharge(_ context.Context, params ChargeParams) (*ChargeResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Idempotent at the provider level, exactly like Stripe.
	if providerID, ok := g.byIdemKey[params.IdempotencyKey]; ok {
		return g.charges[providerID], nil
	}

	sim := params.Metadata["simulate"]
	if sim == SimulateNetwork {
		if !g.netFirst[params.IdempotencyKey] {
			g.netFirst[params.IdempotencyKey] = true
			return nil, NewTransient(errors.New("stub: upstream network timeout"))
		}
		// Retry with the same key settles normally (provider idempotency).
	}
	if sim == SimulateDecline {
		return nil, &PermanentError{Reason: "stub: card declined"}
	}

	g.nextID++
	id := fmt.Sprintf("ch_%06d", g.nextID)
	status := ChargeSucceeded
	if sim == SimulateHold {
		status = ChargePending
	}
	res := &ChargeResult{ID: id, Status: status, Amount: params.Amount, Currency: params.Currency, Last4: "4242"}
	g.charges[id] = res
	g.byIdemKey[params.IdempotencyKey] = id
	return res, nil
}

func (g *stubGateway) GetCharge(_ context.Context, providerChargeID string) (*ChargeResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	res, ok := g.charges[providerChargeID]
	if !ok {
		return nil, errors.New("stub: unknown charge " + providerChargeID)
	}
	return res, nil
}

func (g *stubGateway) RefundCharge(_ context.Context, params RefundParams) (*RefundResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if providerID, ok := g.refundByKey[params.IdempotencyKey]; ok {
		return g.refunds[providerID], nil
	}
	g.nextID++
	id := fmt.Sprintf("re_%06d", g.nextID)
	res := &RefundResult{ID: id, Status: RefundSucceeded}
	g.refunds[id] = res
	g.refundByKey[params.IdempotencyKey] = id
	return res, nil
}

func (g *stubGateway) GetRefund(_ context.Context, providerRefundID string) (*RefundResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	res, ok := g.refunds[providerRefundID]
	if !ok {
		return nil, errors.New("stub: unknown refund " + providerRefundID)
	}
	return res, nil
}

// logGateway keeps a compile-time reference for structured logging tweaks.
var _ = slog.Info
