// Command server runs the shopping sample: PostgreSQL + Redis, chi router,
// embedded migrations and seed data, all in one binary.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bobhuang1/GoLang/internal/api"
	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/cache"
	"github.com/bobhuang1/GoLang/internal/cart"
	"github.com/bobhuang1/GoLang/internal/config"
	"github.com/bobhuang1/GoLang/internal/customer"
	"github.com/bobhuang1/GoLang/internal/order"
	"github.com/bobhuang1/GoLang/internal/payment"
	"github.com/bobhuang1/GoLang/internal/product"
	"github.com/bobhuang1/GoLang/internal/shipping"
	"github.com/bobhuang1/GoLang/internal/store"
)

func main() {
	cfg := config.FromEnv()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("database setup failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// The cache. Redis is preferred; when it is unreachable the service starts
	// degraded (a no-op cache) exactly as the retry/backoff layer documents.
	var cacheStore cache.Cache
	cacheStore = cache.NewRedis(cfg.RedisAddr, cfg.RedisPassword, cache.DefaultOptions())
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	if redis, ok := cacheStore.(*cache.Redis); ok && redis.Ping(pingCtx) == nil {
		cancel()
		slog.Info("redis connected", "addr", cfg.RedisAddr)
	} else {
		if ok {
			cancel()
		} else {
			cancel()
		}
		slog.Warn("redis unreachable, starting with no-op cache (degraded)", "addr", cfg.RedisAddr)
		cacheStore = cache.Null{}
	}

	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTTTL, cfg.JWTChallengeTTL)

	customers := customer.NewService(pool, cacheStore)
	products := product.NewService(pool, cacheStore)
	carts := cart.NewService(pool, products)
	orders := order.NewService(pool)
	gateway := payment.NewStubGateway()
	payments := payment.NewService(pool, cacheStore, gateway, cfg.MaxPaymentAttempts, cfg.PaymentRetryBaseDelay)
	shippings := shipping.NewService(pool)

	// Demo accounts have a published password, so they are only created when
	// explicitly asked for (make run / docker compose set SEED_DEMO=1).
	if os.Getenv("SEED_DEMO") == "1" {
		if err := seed(ctx, pool, customers, products); err != nil {
			slog.Error("seed failed", "err", err)
			os.Exit(1)
		}
	}

	handler := api.NewRouter(api.Deps{
		Pool:      pool,
		Cache:     cacheStore,
		Tokens:    tokens,
		Customers: customers,
		Products:  products,
		Carts:     carts,
		Orders:    orders,
		Payments:  payments,
		Shippings: shippings,
		IdemTTL:   10 * time.Minute,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown incomplete", "err", err)
	}
	slog.Info("server stopped")
}
