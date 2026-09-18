// Package api assembles the HTTP router and all route groups.
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/cache"
	"github.com/bobhuang1/GoLang/internal/cart"
	"github.com/bobhuang1/GoLang/internal/customer"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/bobhuang1/GoLang/internal/order"
	"github.com/bobhuang1/GoLang/internal/payment"
	"github.com/bobhuang1/GoLang/internal/product"
	"github.com/bobhuang1/GoLang/internal/shipping"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps carries every dependency the router needs. cmd/server builds it and the
// wiring stays explicit (single binary, domains as packages).
type Deps struct {
	Pool      *pgxpool.Pool
	Cache     cache.Cache
	Tokens    *auth.TokenManager
	Customers *customer.Service
	Products  *product.Service
	Carts     *cart.Service
	Orders    *order.Service
	Payments  *payment.Service
	Shippings *shipping.Service
	IdemTTL   time.Duration
}

// NewRouter assembles the full HTTP handler.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestIDMW, httpx.RecoverMW, httpx.LoggerMW)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.Pool.Ping(ctx); err != nil {
			httpx.WriteError(w, httpx.Internal("database unreachable", err))
			return
		}
		httpx.WriteOK(w, map[string]string{"status": "ok"})
	})

	// In-memory replay guard for idempotent POST endpoints (payments,
	// checkout). The durable guarantee is the DB unique keys; this layer only
	// short-circuits safe retries.
	guard := httpx.NewIdempotencyGuard(d.IdemTTL, 1000)

	r.Route("/api/v1", func(r chi.Router) {
		// ---- auth + customers ----
		customer.NewHandler(d.Customers, d.Tokens).Routes(r)

		// ---- products ----
		productH := product.NewHandler(d.Products)
		r.Route("/products", productH.PublicRoutes)
		r.Route("/admin/products", func(r chi.Router) {
			productH.AdminRoutes(r, d.Tokens)
		})

		// ---- cart ----
		cartH := cart.NewHandler(d.Carts, d.Tokens)
		r.Route("/cart", func(r chi.Router) {
			r.Use(guard.Middleware)
			cartH.Routes(r)
		})

		// ---- orders ----
		orderH := order.NewHandler(d.Orders, d.Pool, d.Tokens)
		r.Route("/orders", orderH.CustomerRoutes)
		r.Route("/admin/orders", orderH.AdminRoutes)

		// ---- payments ----
		paymentH := payment.NewHandler(d.Payments, d.Tokens)
		r.Route("/payments", func(r chi.Router) {
			r.Use(guard.Middleware)
			paymentH.CustomerRoutes(r)
		})
		r.Route("/admin/refunds", func(r chi.Router) {
			r.Use(guard.Middleware)
			paymentH.AdminRoutes(r)
		})

		// ---- shipping ----
		shippingH := shipping.NewHandler(d.Shippings, d.Pool, d.Tokens)
		r.Route("/shipping", shippingH.CustomerRoutes)
		r.Route("/admin/shipments", shippingH.AdminRoutes)
	})

	return r
}
