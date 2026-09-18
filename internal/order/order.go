// Package order implements orders, their status lifecycle and the admin
// processing endpoints. The status transitions are enforced by a concurrency-
// safe state machine on top of PostgreSQL row locks.
package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is the lifecycle state of an order.
type Status string

// Known order statuses. They match the Postgres order_status enum.
const (
	StatusPending    Status = "pending"
	StatusPaid       Status = "paid"
	StatusProcessing Status = "processing"
	StatusShipped    Status = "shipped"
	StatusDelivered  Status = "delivered"
	StatusCancelled  Status = "cancelled"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusPaid, StatusProcessing, StatusShipped, StatusDelivered, StatusCancelled:
		return true
	}
	return false
}

// Order is the core aggregate root.
type Order struct {
	ID         int64     `json:"id"`
	CustomerID int64     `json:"customer_id"`
	Status     Status    `json:"status"`
	TotalCents int64     `json:"total_cents"`
	Currency   string    `json:"currency"`
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Item is one ordered product line.
type Item struct {
	ProductID      int64  `json:"product_id"`
	ProductName    string `json:"product_name"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	Qty            int    `json:"qty"`
}

// View bundles an order with its lines.
type View struct {
	Order
	Items []Item `json:"items"`
}

// Service implements the order lifecycle rules.
type Service struct {
	pool *pgxpool.Pool
}

// NewService wires the order domain to Postgres.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Create inserts a new order (used by the cart checkout) inside the caller's
// transaction and returns the created order row.
func Create(ctx context.Context, tx pgx.Tx, customerID int64, totalCents int64, currency, idemKey string) (*Order, error) {
	var o Order
	err := tx.QueryRow(ctx, `
		INSERT INTO orders (customer_id, total_cents, currency, idempotency_key)
		VALUES ($1,$2,$3,$4)
		RETURNING id, customer_id, status, total_cents, currency, version, created_at, updated_at`,
		customerID, totalCents, currency, nullableString(idemKey),
	).Scan(&o.ID, &o.CustomerID, &o.Status, &o.TotalCents, &o.Currency, &o.Version, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// AddItem inserts an order line inside the caller's transaction.
func AddItem(ctx context.Context, tx pgx.Tx, orderID, productID int64, productName string, unitPriceCents int64, qty int) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO order_items (order_id, product_id, product_name, unit_price_cents, qty)
		VALUES ($1,$2,$3,$4,$5)`,
		orderID, productID, productName, unitPriceCents, qty)
	return err
}

// FindOrderByIDKey returns an order matching an idempotency key, if any.
func (s *Service) FindOrderByIDKey(ctx context.Context, idemKey string) (*Order, error) {
	var o Order
	err := s.pool.QueryRow(ctx, `
		SELECT id, customer_id, status, total_cents, currency, version, created_at, updated_at
		FROM orders WHERE idempotency_key = $1`, idemKey,
	).Scan(&o.ID, &o.CustomerID, &o.Status, &o.TotalCents, &o.Currency, &o.Version, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("order not found")
		}
		return nil, httpx.Wrap(err)
	}
	return &o, nil
}

// ByID loads an order.
func (s *Service) ByID(ctx context.Context, orderID int64) (*Order, error) {
	var o Order
	err := s.pool.QueryRow(ctx, `
		SELECT id, customer_id, status, total_cents, currency, version, created_at, updated_at
		FROM orders WHERE id = $1`, orderID,
	).Scan(&o.ID, &o.CustomerID, &o.Status, &o.TotalCents, &o.Currency, &o.Version, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("order not found")
		}
		return nil, httpx.Wrap(err)
	}
	return &o, nil
}

// Items loads the lines of an order.
func (s *Service) Items(ctx context.Context, orderID int64) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT product_id, product_name, unit_price_cents, qty
		FROM order_items WHERE order_id = $1 ORDER BY product_id`, orderID)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ProductID, &it.ProductName, &it.UnitPriceCents, &it.Qty); err != nil {
			return nil, httpx.Wrap(err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// View1 returns a full order view (order + lines).
func (s *Service) View1(ctx context.Context, orderID int64) (*View, error) {
	o, err := s.ByID(ctx, orderID)
	if err != nil {
		return nil, err
	}
	items, err := s.Items(ctx, orderID)
	if err != nil {
		return nil, err
	}
	return &View{Order: *o, Items: items}, nil
}

// Own lists the orders of the calling customer.
func (s *Service) Own(ctx context.Context, customerID int64) ([]View, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, customer_id, status, total_cents, currency, version, created_at, updated_at
		FROM orders WHERE customer_id = $1 ORDER BY id DESC`, customerID)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer rows.Close()

	var views []View
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.CustomerID, &o.Status, &o.TotalCents, &o.Currency,
			&o.Version, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, httpx.Wrap(err)
		}
		items, err := s.Items(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		views = append(views, View{Order: o, Items: items})
	}
	return views, rows.Err()
}

// List returns orders for the admin console, optionally filtered by status.
func (s *Service) List(ctx context.Context, status Status, page, limit int) ([]View, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := (page - 1) * limit
	statusFilter := ""
	if status.Valid() {
		statusFilter = string(status)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, customer_id, status, total_cents, currency, version, created_at, updated_at
		FROM orders
		WHERE $3 = '' OR status = $3::order_status
		ORDER BY id DESC
		LIMIT $1 OFFSET $2`, limit, offset, statusFilter)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer rows.Close()

	var views []View
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.CustomerID, &o.Status, &o.TotalCents, &o.Currency,
			&o.Version, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, httpx.Wrap(err)
		}
		views = append(views, View{Order: o})
	}
	return views, rows.Err()
}

// Action is an admin order-processing step.
type Action string

// Admin actions accepted by Process.
const (
	ActionConfirm Action = "confirm" // paid -> processing
	ActionCancel  Action = "cancel"  // pending/paid/processing -> cancelled
)

// Process applies an admin action to an order, guarded inside a transaction.
// Two concurrent requests race on the FOR UPDATE row lock: the second will see
// the new status and be rejected if the transition is no longer legal.
func (s *Service) Process(ctx context.Context, orderID int64, action Action) (*View, error) {
	if action != ActionConfirm && action != ActionCancel {
		return nil, httpx.BadRequest("unknown action; allowed: confirm, cancel")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var o Order
	if err := tx.QueryRow(ctx, `
		SELECT id, customer_id, status, total_cents, currency, version, created_at, updated_at
		FROM orders WHERE id = $1 FOR UPDATE`, orderID,
	).Scan(&o.ID, &o.CustomerID, &o.Status, &o.TotalCents, &o.Currency, &o.Version, &o.CreatedAt, &o.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("order not found")
		}
		return nil, httpx.Wrap(err)
	}

	next, ok := transition(o.Status, action)
	if !ok {
		return nil, httpx.Conflict(fmt.Sprintf(
			"cannot %s an order in status %s", action, o.Status))
	}

	if _, err := tx.Exec(ctx, `
		UPDATE orders SET status = $2, version = version + 1, updated_at = now()
		WHERE id = $1 AND version = $3`,
		o.ID, string(next), o.Version); err != nil {
		return nil, httpx.Wrap(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, httpx.Wrap(err)
	}
	o.Status = next
	o.Version++
	return &View{Order: o}, nil
}

// transition defines the legal (status, action) -> next status edges.
func transition(from Status, action Action) (Status, bool) {
	switch action {
	case ActionConfirm:
		if from == StatusPaid {
			return StatusProcessing, true
		}
	case ActionCancel:
		switch from {
		case StatusPending, StatusPaid, StatusProcessing:
			return StatusCancelled, true
		}
	default:
		return "", false
	}
	return "", false
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
