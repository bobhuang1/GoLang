package main

import (
	"context"
	"log/slog"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/customer"
	"github.com/bobhuang1/GoLang/internal/product"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seed loads demo accounts and products so the sample is runnable out of the
// box. Idempotent: existing rows are left untouched.
func seed(ctx context.Context, pool *pgxpool.Pool, customers *customer.Service, products *product.Service) error {
	adminHash, err := auth.HashPassword("ChangeMe123!")
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO customers (email, password_hash, full_name, is_admin)
		VALUES ('admin@example.test', $1, 'Demo Admin', TRUE)
		ON CONFLICT (email) DO NOTHING`, adminHash); err != nil {
		return err
	}

	custHash, err := auth.HashPassword("ChangeMe123!")
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO customers (email, password_hash, full_name)
		VALUES ('customer@example.test', $1, 'Demo Customer')
		ON CONFLICT (email) DO NOTHING`, custHash); err != nil {
		return err
	}
	slog.Info("seeded demo accounts",
		"admin", "admin@example.test", "customer", "customer@example.test", "password", "ChangeMe123!")

	demoProducts := []struct {
		sku, name, description string
		price                  int64
		stock                  int
	}{
		{"MOUSE-001", "Wireless Mouse", "Silent click, 2.4 GHz", 2499, 50},
		{"KB-001", "Mechanical Keyboard", "65% layout, hot-swap", 8999, 30},
		{"MON-001", "27in 4K Monitor", "IPS, 95% DCI-P3", 39999, 20},
		{"DOCK-001", "USB-C Hub", "8-in-1, 100W PD", 5499, 100},
		{"CAM-001", "Webcam 1080p", "Autofocus, dual mic", 7999, 40},
	}
	for _, p := range demoProducts {
		if _, err := products.Create(ctx, p.sku, p.name, p.description, "usd", p.price, p.stock); err != nil {
			// Duplicate SKU on re-seed is fine.
			slog.Debug("product already seeded (skipping)", "sku", p.sku)
		}
	}
	slog.Info("seeded demo products", "count", len(demoProducts))
	return nil
}
