// Package product implements admin product CRUD plus a public read-through
// cache over Redis (with retry/backoff) for product reads.
package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/bobhuang1/GoLang/internal/cache"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Product is a sellable item.
type Product struct {
	ID          int64     `json:"id"`
	SKU         string    `json:"sku"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	PriceCents  int64     `json:"price_cents"`
	Currency    string    `json:"currency"`
	Stock       int       `json:"stock"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
}

// Service owns the product business rules.
type Service struct {
	pool     *pgxpool.Pool
	cache    cache.Cache
	cacheTTL time.Duration
}

// NewService wires products to Postgres and the (retrying) Redis cache.
func NewService(pool *pgxpool.Pool, c cache.Cache) *Service {
	return &Service{pool: pool, cache: c, cacheTTL: 60 * time.Second}
}

// UpdateInput is the patch body; nil/empty optional fields keep the old value.
type UpdateInput struct {
	SKU         string  `json:"sku"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	PriceCents  *int64  `json:"price_cents"`
	Currency    string  `json:"currency"`
	Stock       *int    `json:"stock"`
}

func cacheKey(id int64) string { return "product:" + strconv.FormatInt(id, 10) }

// Create adds a product and (best-effort) publishes the cache.
func (s *Service) Create(ctx context.Context, sku, name, description, currency string, priceCents int64, stock int) (*Product, error) {
	if sku == "" || name == "" || priceCents < 0 || stock < 0 {
		return nil, httpx.BadRequest("sku, name, non-negative price and stock are required")
	}
	if currency == "" {
		currency = "usd"
	}
	var p Product
	err := s.pool.QueryRow(ctx, `
		INSERT INTO products (sku, name, description, price_cents, currency, stock)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, sku, name, description, price_cents, currency, stock, active, created_at`,
		sku, name, description, priceCents, currency, stock,
	).Scan(&p.ID, &p.SKU, &p.Name, &p.Description, &p.PriceCents, &p.Currency, &p.Stock, &p.Active, &p.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, httpx.Conflict("sku already exists")
		}
		return nil, httpx.Wrap(err)
	}
	s.cacheSet(ctx, &p)
	return &p, nil
}

// Get reads a product through the cache: hit -> JSON, miss -> DB + backfill.
// A Redis failure degrades to PostgreSQL instead of failing the request.
func (s *Service) Get(ctx context.Context, id int64) (*Product, error) {
	if raw, err := s.cache.Get(ctx, cacheKey(id)); err == nil {
		var p Product
		if json.Unmarshal(raw, &p) == nil {
			return &p, nil
		}
	} else if !errors.Is(err, cache.ErrMiss) {
		slog.Warn("product cache read degraded to postgres", "product_id", id, "err", err)
	}

	var p Product
	err := s.pool.QueryRow(ctx, `
		SELECT id, sku, name, description, price_cents, currency, stock, active, created_at
		FROM products WHERE id = $1 AND active = TRUE`, id,
	).Scan(&p.ID, &p.SKU, &p.Name, &p.Description, &p.PriceCents, &p.Currency, &p.Stock, &p.Active, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("product not found")
		}
		return nil, httpx.Wrap(err)
	}
	s.cacheSet(ctx, &p)
	return &p, nil
}

// List returns active products with basic pagination.
func (s *Service) List(ctx context.Context, page, limit int, query string) ([]Product, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := (page - 1) * limit

	rows, err := s.pool.Query(ctx, `
		SELECT id, sku, name, description, price_cents, currency, stock, active, created_at
		FROM products
		WHERE active = TRUE AND ($3 = '' OR name ILIKE $3 OR sku ILIKE $3 OR description ILIKE $3)
		ORDER BY id
		LIMIT $1 OFFSET $2`, limit, offset, likePattern(query))
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	defer rows.Close()

	var out []Product
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.SKU, &p.Name, &p.Description, &p.PriceCents,
			&p.Currency, &p.Stock, &p.Active, &p.CreatedAt); err != nil {
			return nil, httpx.Wrap(err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Update modifies a product and invalidates its cache entry.
func (s *Service) Update(ctx context.Context, id int64, in UpdateInput) (*Product, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE products SET
			sku            = COALESCE(NULLIF($2, ''), sku),
			name           = COALESCE(NULLIF($3, ''), name),
			description    = COALESCE($4, description),
			price_cents    = COALESCE($5, price_cents),
			currency       = COALESCE(NULLIF($6, ''), currency),
			stock          = COALESCE($7, stock),
			updated_at     = now()
		WHERE id = $1`,
		id, in.SKU, in.Name, in.Description, in.PriceCents, in.Currency, in.Stock)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("product not found")
	}
	// Best-effort cache invalidation; failure only leaves a stale TTL window.
	if err := s.cache.Del(ctx, cacheKey(id)); err != nil {
		slog.Warn("product cache invalidation degraded", "product_id", id, "err", err)
	}
	return s.Get(ctx, id)
}

// Deactivate soft-deletes a product (active = FALSE) and invalidates the cache.
func (s *Service) Deactivate(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE products SET active = FALSE, updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("product not found")
	}
	if err := s.cache.Del(ctx, cacheKey(id)); err != nil {
		slog.Warn("product cache invalidation degraded", "product_id", id, "err", err)
	}
	return nil
}

// ReserveStock decrements stock inside the caller's transaction. The row lock
// (SELECT ... FOR UPDATE) plus the CHECK (stock >= 0) serialise concurrent
// checkout attempts and make overselling impossible.
func ReserveStock(ctx context.Context, tx pgx.Tx, productID int64, qty int) (*Product, error) {
	var p Product
	err := tx.QueryRow(ctx, `
		SELECT id, sku, name, price_cents, currency, stock, active
		FROM products WHERE id = $1 FOR UPDATE`, productID,
	).Scan(&p.ID, &p.SKU, &p.Name, &p.PriceCents, &p.Currency, &p.Stock, &p.Active)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("product not found")
		}
		return nil, httpx.Wrap(err)
	}
	if !p.Active {
		return nil, httpx.Conflict("product is no longer active")
	}
	if p.Stock < qty {
		return nil, httpx.Conflict(fmt.Sprintf(
			"insufficient stock for %s (have %d, want %d)", p.Name, p.Stock, qty))
	}
	if _, err := tx.Exec(ctx,
		`UPDATE products SET stock = stock - $2, updated_at = now() WHERE id = $1`, productID, qty); err != nil {
		return nil, httpx.Wrap(err)
	}
	p.Stock -= qty
	return &p, nil
}

func (s *Service) cacheSet(ctx context.Context, p *Product) {
	raw, err := json.Marshal(p)
	if err != nil {
		return
	}
	if err := s.cache.Set(ctx, cacheKey(p.ID), raw, s.cacheTTL); err != nil {
		slog.Warn("product cache write degraded", "product_id", p.ID, "err", err)
	}
}

func likePattern(q string) string {
	if q == "" {
		return ""
	}
	return "%" + q + "%"
}
