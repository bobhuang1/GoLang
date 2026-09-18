// Package shipping implements shipment creation, tracking and the admin
// fulfilment workflow. Orders move shipped->delivered as the shipment's status
// advances; every write is serialised by a FOR UPDATE order lock.
package shipping

import (
	"context"
	"errors"
	"time"

	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status mirrors the DB shipment_status enum.
type Status string

// Shipment statuses.
const (
	StatusPending   Status = "pending"
	StatusPicked    Status = "picked"
	StatusInTransit Status = "in_transit"
	StatusDelivered Status = "delivered"
)

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusPicked, StatusInTransit, StatusDelivered:
		return true
	}
	return false
}

// Shipment is the tracking record.
type Shipment struct {
	ID           string    `json:"id"`
	OrderID      int64     `json:"order_id"`
	TrackingCode string    `json:"tracking_code"`
	Courier      string    `json:"courier"`
	Status       Status    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Service implements the shipping rules.
type Service struct {
	pool *pgxpool.Pool
}

// NewService wires shipping to Postgres.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Ship creates a shipment for an order and moves the order to 'shipped'.
// The order must be 'processing' or 'paid' (i.e. payment settled). Both the
// shipment insert and the order transition share one transaction.
func (s *Service) Ship(ctx context.Context, orderID int64, courier, trackingCode string) (*Shipment, error) {
	if courier == "" || trackingCode == "" {
		return nil, httpx.BadRequest("courier and tracking_code are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var oStatus string
	var oVersion int
	if err := tx.QueryRow(ctx, `
		SELECT status, version FROM orders WHERE id = $1 FOR UPDATE`, orderID,
	).Scan(&oStatus, &oVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("order not found")
		}
		return nil, httpx.Wrap(err)
	}
	if oStatus != "processing" && oStatus != "paid" {
		return nil, httpx.Conflict("order must be paid or processing before shipping")
	}

	var sh Shipment
	if err := tx.QueryRow(ctx, `
		INSERT INTO shipments (order_id, tracking_code, courier, status)
		VALUES ($1,$2,$3,'pending')
		RETURNING id::text, order_id, tracking_code, courier, status, created_at, updated_at`,
		orderID, trackingCode, courier,
	).Scan(&sh.ID, &sh.OrderID, &sh.TrackingCode, &sh.Courier, &sh.Status, &sh.CreatedAt, &sh.UpdatedAt); err != nil {
		return nil, httpx.Wrap(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE orders SET status = 'shipped', version = version + 1, updated_at = now()
		WHERE id = $1 AND status = $2 AND version = $3`, orderID, oStatus, oVersion); err != nil {
		return nil, httpx.Wrap(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, httpx.Wrap(err)
	}
	return &sh, nil
}

// Update changes a shipment's status (and optionally the tracking code). When
// the shipment is delivered the order moves to 'delivered' as well.
func (s *Service) Update(ctx context.Context, shipmentID, trackingCode, status string) (*Shipment, error) {
	if !Status(status).Valid() {
		return nil, httpx.BadRequest("unknown shipment status")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sh Shipment
	if err := tx.QueryRow(ctx, `
		SELECT id::text, order_id, tracking_code, courier, status, created_at, updated_at
		FROM shipments WHERE id = $1::uuid FOR UPDATE`, shipmentID,
	).Scan(&sh.ID, &sh.OrderID, &sh.TrackingCode, &sh.Courier, &sh.Status, &sh.CreatedAt, &sh.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("shipment not found")
		}
		return nil, httpx.Wrap(err)
	}
	next := Status(status)
	if !transitionAllowed(sh.Status, next) {
		return nil, httpx.Conflict("invalid shipment status transition")
	}
	if trackingCode != "" {
		sh.TrackingCode = trackingCode
	}
	if _, err := tx.Exec(ctx, `
		UPDATE shipments SET status = $2::shipment_status, tracking_code = $3,
		       updated_at = now()
		WHERE id = $1::uuid`, shipmentID, string(next), sh.TrackingCode); err != nil {
		return nil, httpx.Wrap(err)
	}
	if next == StatusDelivered {
		if _, err := tx.Exec(ctx, `
			UPDATE orders SET status = 'delivered', version = version + 1, updated_at = now()
			WHERE id = $1 AND status = 'shipped'`, sh.OrderID); err != nil {
			return nil, httpx.Wrap(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, httpx.Wrap(err)
	}
	sh.Status = next
	return &sh, nil
}

// ByOrder returns the shipment for an order (with caller authz in the handler
// via the owning customer).
func (s *Service) ByOrder(ctx context.Context, orderID int64) (*Shipment, error) {
	var sh Shipment
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, order_id, tracking_code, courier, status, created_at, updated_at
		FROM shipments WHERE order_id = $1`, orderID,
	).Scan(&sh.ID, &sh.OrderID, &sh.TrackingCode, &sh.Courier, &sh.Status, &sh.CreatedAt, &sh.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("no shipment for this order")
		}
		return nil, httpx.Wrap(err)
	}
	return &sh, nil
}

// Own lists the caller's shipments.
func (s *Service) Own(ctx context.Context, customerID int64) ([]*Shipment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id::text, s.order_id, s.tracking_code, s.courier, s.status, s.created_at, s.updated_at
		FROM shipments s JOIN orders o ON o.id = s.order_id
		WHERE o.customer_id = $1 ORDER BY s.created_at DESC`, customerID)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer rows.Close()
	var out []*Shipment
	for rows.Next() {
		var sh Shipment
		if err := rows.Scan(&sh.ID, &sh.OrderID, &sh.TrackingCode, &sh.Courier,
			&sh.Status, &sh.CreatedAt, &sh.UpdatedAt); err != nil {
			return nil, httpx.Wrap(err)
		}
		out = append(out, &sh)
	}
	return out, rows.Err()
}

// Track looks up a shipment by its public tracking code.
func (s *Service) Track(ctx context.Context, code string) (*Shipment, error) {
	var sh Shipment
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, order_id, tracking_code, courier, status, created_at, updated_at
		FROM shipments WHERE tracking_code = $1`, code,
	).Scan(&sh.ID, &sh.OrderID, &sh.TrackingCode, &sh.Courier, &sh.Status, &sh.CreatedAt, &sh.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("tracking code not found")
		}
		return nil, httpx.Wrap(err)
	}
	return &sh, nil
}

// transitionAllowed guards the shipment state machine.
func transitionAllowed(from, to Status) bool {
	if from == to {
		return true
	}
	switch from {
	case StatusPending:
		return to == StatusPicked || to == StatusInTransit
	case StatusPicked:
		return to == StatusInTransit
	case StatusInTransit:
		return to == StatusDelivered
	case StatusDelivered:
		return false
	}
	return false
}
