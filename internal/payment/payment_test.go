package payment

import (
	"context"
	"testing"
)

func TestStubChargeSucceeds(t *testing.T) {
	g := NewStubGateway()
	res, err := g.CreateCharge(context.Background(), ChargeParams{
		Amount: 1000, Currency: "usd", IdempotencyKey: "k-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != ChargeSucceeded {
		t.Fatalf("status = %s, want succeeded", res.Status)
	}
	if res.Amount != 1000 || res.Currency != "usd" {
		t.Fatalf("amount/currency mismatch: %d %s", res.Amount, res.Currency)
	}
}

func TestStubDeclineIsPermanent(t *testing.T) {
	g := NewStubGateway()
	_, err := g.CreateCharge(context.Background(), ChargeParams{
		IdempotencyKey: "k-decline",
		Metadata:       map[string]string{"simulate": SimulateDecline},
	})
	if err == nil {
		t.Fatal("expected decline error")
	}
	if IsTransient(err) {
		t.Fatalf("decline must be permanent, got transient: %v", err)
	}
}

func TestStubNetworkIsTransientFirstCall(t *testing.T) {
	g := NewStubGateway()
	params := ChargeParams{
		IdempotencyKey: "k-net",
		Metadata:       map[string]string{"simulate": SimulateNetwork},
	}
	if _, err := g.CreateCharge(context.Background(), params); !IsTransient(err) {
		t.Fatalf("first network call should be transient, got %v", err)
	}
	// Retry with the same key: provider is idempotent and now succeeds.
	res, err := g.CreateCharge(context.Background(), params)
	if err != nil || res.Status != ChargeSucceeded {
		t.Fatalf("retry should succeed, got %v %v", res, err)
	}
}

func TestStubChargeIdempotentByKey(t *testing.T) {
	g := NewStubGateway()
	params := ChargeParams{Amount: 2500, Currency: "usd", IdempotencyKey: "k-dup"}
	first, err := g.CreateCharge(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := g.CreateCharge(context.Background(), params)
		if err != nil {
			t.Fatalf("replay %d failed: %v", i, err)
		}
		if again.ID != first.ID {
			t.Fatalf("replay %d returned different provider id %s != %s", i, again.ID, first.ID)
		}
	}
}

func TestStubRefundSucceeds(t *testing.T) {
	g := NewStubGateway()
	got, err := g.RefundCharge(context.Background(), RefundParams{
		ChargeID: "ch_000001", Amount: 500, IdempotencyKey: "r-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Status != RefundSucceeded {
		t.Fatalf("status = %s, want succeeded", got.Status)
	}
}
