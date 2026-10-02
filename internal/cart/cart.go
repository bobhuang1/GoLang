// Package cart implements the shopping cart and the transactional checkout.
// Checkout is the demo of "keep reads parallel (errgroup) and writes serial":
// the order is created in a single Postgres transaction where product stock is
// reserved with FOR UPDATE row locks, so concurrent checkouts cannot oversell.
package cart

import (
	"context"
	"errors"
	"sync"

	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/bobhuang1/GoLang/internal/order"
	"github.com/bobhuang1/GoLang/internal/product"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

// Item is a single cart line (product + quantity).
type Item struct {
	ProductID int64 `json:"product_id"`
	Qty       int   `json:"qty"`
}

// ViewItem is a cart line enriched with product data.
type ViewItem struct {
	ProductID      int64  `json:"product_id"`
	SKU            string `json:"sku"`
	Name           string `json:"name"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	Currency       string `json:"currency"`
	Qty            int    `json:"qty"`
}

// View is the full cart the customer sees.
type View struct {
	Items      []ViewItem `json:"items"`
	TotalCents int64      `json:"total_cents"`
	Currency   string     `json:"currency"`
	Count      int        `json:"count"`
}

// Service implements the cart rules.
type Service struct {
	pool    *pgxpool.Pool
	product *product.Service
}

// NewService wires the cart to Postgres and the product store.
func NewService(pool *pgxpool.Pool, products *product.Service) *Service {
	return &Service{pool: pool, product: products}
}

// View returns the customer's cart, enriching product lines concurrently.
func (s *Service) View(ctx context.Context, customerID int64) (*View, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT product_id, qty FROM cart_items
		WHERE customer_id = $1 ORDER BY product_id`, customerID)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ProductID, &it.Qty); err != nil {
			return nil, httpx.Wrap(err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, httpx.Wrap(err)
	}

	// Parallel reads: every distinct product is resolved through the
	// cache-backed product store concurrently via errgroup.
	var mu sync.Mutex
	products := make(map[int64]*product.Product, len(items))
	var g errgroup.Group
	for _, it := range items {
		it := it
		g.Go(func() error {
			p, err := s.product.Get(ctx, it.ProductID)
			if err != nil {
				return err
			}
			mu.Lock()
			products[p.ID] = p
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	out := &View{}
	for _, it := range items {
		p, ok := products[it.ProductID]
		if !ok {
			continue // product vanished; cart line silently skipped
		}
		lineTotal := p.PriceCents * int64(it.Qty)
		out.Items = append(out.Items, ViewItem{
			ProductID:      p.ID,
			SKU:            p.SKU,
			Name:           p.Name,
			UnitPriceCents: p.PriceCents,
			Currency:       p.Currency,
			Qty:            it.Qty,
		})
		out.TotalCents += lineTotal
		out.Count += it.Qty
		out.Currency = p.Currency
	}
	return out, nil
}

// Add inserts or updates a line (absolute quantity merge).
func (s *Service) Add(ctx context.Context, customerID, productID int64, qty int) error {
	if qty < 1 {
		return httpx.BadRequest("quantity must be >= 1")
	}
	_, err := s.product.Get(ctx, productID)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO cart_items (customer_id, product_id, qty)
		VALUES ($1,$2,$3)
		ON CONFLICT (customer_id, product_id)
		DO UPDATE SET qty = excluded.qty, updated_at = now()`,
		customerID, productID, qty); err != nil {
		return httpx.Wrap(err)
	}
	return nil
}

// Update sets the quantity of a line (0 or negative deletes it).
func (s *Service) Update(ctx context.Context, customerID, productID int64, qty int) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cart_items SET qty = $3, updated_at = now()
		WHERE customer_id = $1 AND product_id = $2`, customerID, productID, qty)
	if err != nil {
		return httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("product not in cart")
	}
	return nil
}

// Remove deletes a line.
func (s *Service) Remove(ctx context.Context, customerID, productID int64) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM cart_items WHERE customer_id = $1 AND product_id = $2`, customerID, productID)
	if err != nil {
		return httpx.Wrap(err)
	}
	return nil
}

// Clear empties the cart.
func (s *Service) Clear(ctx context.Context, customerID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM cart_items WHERE customer_id = $1`, customerID)
	if err != nil {
		return httpx.Wrap(err)
	}
	return nil
}

// Checkout turns the cart into an order inside one transaction. Stock is
// reserved line-by-line with FOR UPDATE; any failure rolls everything back.
// The order's idempotency_key makes the whole operation replay-safe.
func (s *Service) Checkout(ctx context.Context, customerID int64, idemKey string) (*order.View, error) {
	if idemKey == "" {
		return nil, httpx.BadRequest("Idempotency-Key header is required for checkout")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT product_id, qty FROM cart_items
		WHERE customer_id = $1 ORDER BY product_id`, customerID)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ProductID, &it.Qty); err != nil {
			rows.Close()
			return nil, httpx.Wrap(err)
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, httpx.Wrap(err)
	}
	if len(items) == 0 {
		return nil, httpx.BadRequest("cart is empty")
	}

	// Reserve stock and compute the total, all inside the transaction.
	currency := "usd"
	var total int64
	reserved := make([]order.Item, 0, len(items))
	for _, it := range items {
		p, err := product.ReserveStock(ctx, tx, it.ProductID, it.Qty)
		if err != nil {
			return nil, err
		}
		total += p.PriceCents * int64(it.Qty)
		currency = p.Currency
		reserved = append(reserved, order.Item{
			ProductID: p.ID, ProductName: p.Name, UnitPriceCents: p.PriceCents, Qty: it.Qty,
		})
	}

	o, err := order.Create(ctx, tx, customerID, total, currency, idemKey)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Same idempotency key: replay the already-created order instead of
			// duplicating it.
			return s.replayOrder(ctx, customerID, idemKey)
		}
		return nil, httpx.Wrap(err)
	}
	for _, it := range reserved {
		if err := order.AddItem(ctx, tx, o.ID, it.ProductID, it.ProductName, it.UnitPriceCents, it.Qty); err != nil {
			return nil, httpx.Wrap(err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cart_items WHERE customer_id = $1`, customerID); err != nil {
		return nil, httpx.Wrap(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, httpx.Wrap(err)
	}

	return &order.View{Order: *o, Items: reserved}, nil
}

// replayOrder returns this customer's already-created order for a repeated checkout
// key. Keys are client-chosen and unique per customer, so the lookup is scoped to the
// caller: another customer's order is never returned.
func (s *Service) replayOrder(ctx context.Context, customerID int64, idemKey string) (*order.View, error) {
	var o order.Order
	err := s.pool.QueryRow(ctx, `
		SELECT id, customer_id, status, total_cents, currency, version, created_at, updated_at
		FROM orders WHERE idempotency_key = $1 AND customer_id = $2`, idemKey, customerID,
	).Scan(&o.ID, &o.CustomerID, &o.Status, &o.TotalCents, &o.Currency, &o.Version, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("order for this key was rolled back")
		}
		return nil, httpx.Wrap(err)
	}
	items, err := s.linesForOrder(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	return &order.View{Order: o, Items: items}, nil
}

func (s *Service) linesForOrder(ctx context.Context, orderID int64) ([]order.Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT product_id, product_name, unit_price_cents, qty
		FROM order_items WHERE order_id = $1 ORDER BY product_id`, orderID)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer rows.Close()
	var out []order.Item
	for rows.Next() {
		var it order.Item
		if err := rows.Scan(&it.ProductID, &it.ProductName, &it.UnitPriceCents, &it.Qty); err != nil {
			return nil, httpx.Wrap(err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
