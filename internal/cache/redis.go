// Package cache provides a Redis-backed cache with exponential backoff on the
// wire operations. Redis is never treated as a source of truth: callers that
// fail here are expected to degrade to PostgreSQL and retry.
package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrMiss reports a cache read that did not hit.
var ErrMiss = errors.New("cache miss")

// Options tunes the retry/backoff behaviour shared by all cache operations.
type Options struct {
	MaxAttempts int
	BaseDelay   time.Duration
	OpTimeout   time.Duration
}

// DefaultOptions is the sane default for a local demo stack.
func DefaultOptions() Options {
	return Options{MaxAttempts: 3, BaseDelay: 40 * time.Millisecond, OpTimeout: 500 * time.Millisecond}
}

// BackoffDelay returns the exponential delay (with jitter) for the given
// attempt index: base*2^attempt spread by +/-20%. Used both by the cache
// wrapper and by the payment-restart loop.
func BackoffDelay(attempt int, base time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	pure := float64(base) * float64(uint64(1)<<uint(attempt))
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(pure * jitter)
}

// Cache is the storage-agnostic interface every domain uses.
type Cache interface {
	// Get returns the stored bytes or ErrMiss.
	Get(ctx context.Context, key string) ([]byte, error)
	// Set stores a value with a TTL.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Del removes one or more keys.
	Del(ctx context.Context, key ...string) error
}

// Redis implements Cache over go-redis with retry/backoff on every operation.
type Redis struct {
	client *redis.Client
	opts   Options
}

// NewRedis connects (lazily) to Redis. A failed connect only warns: the app
// starts degraded and recovers as soon as Redis is reachable.
func NewRedis(addr, password string, opts Options) *Redis {
	return &Redis{
		client: redis.NewClient(&redis.Options{Addr: addr, Password: password}),
		opts:   opts,
	}
}

func (r *Redis) retry(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt < r.opts.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := BackoffDelay(attempt-1, r.opts.BaseDelay)
			slog.Warn("cache op failed, retrying", "op", op, "attempt", attempt+1, "delay_ms", delay.Milliseconds(), "err", lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		opCtx, cancel := context.WithTimeout(ctx, r.opts.OpTimeout)
		err := fn(opCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("cache %s: %w", op, lastErr)
}

// Get reads a key with retry/backoff and maps a miss to ErrMiss.
func (r *Redis) Get(ctx context.Context, key string) ([]byte, error) {
	var out []byte
	err := r.retry(ctx, "get", func(ctx context.Context) error {
		val, err := r.client.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			return ErrMiss
		}
		if err != nil {
			return err
		}
		out = val
		return nil
	})
	return out, err
}

// Set writes a key with retry/backoff.
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.retry(ctx, "set", func(ctx context.Context) error {
		return r.client.Set(ctx, key, value, ttl).Err()
	})
}

// Del removes keys with retry/backoff.
func (r *Redis) Del(ctx context.Context, key ...string) error {
	return r.retry(ctx, "del", func(ctx context.Context) error {
		return r.client.Del(ctx, key...).Err()
	})
}

// Ping checks connectivity (used by the health endpoint).
func (r *Redis) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

// Close releases the underlying client.
func (r *Redis) Close() error {
	return r.client.Close()
}

// Null is a no-op cache: every op succeeds and reads always miss. It lets the
// service run fully degraded when Redis is absent.
type Null struct{}

func (Null) Get(ctx context.Context, key string) ([]byte, error) { return nil, ErrMiss }
func (Null) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return nil
}
func (Null) Del(ctx context.Context, key ...string) error { return nil }
