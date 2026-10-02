# GoLang — shopping-site microservice sample (Go)

A single-binary REST backend for a generic shopping site that reads from Redis
and writes to PostgreSQL. Built to demonstrate idiomatic Go concurrency across
domains while keeping every stateful operation safe.

- **Stack:** Go 1.27, `chi` router, `pgx` (pgxpool), `go-redis/v9`, JWT (HS256),
  TOTP 2FA, bcrypt passwords. Migrations are embedded and applied at startup.
- **No framework magic:** domains are plain packages under `internal/`; the
  wiring lives in `cmd/server` and `internal/api`.

## What it does

| Area | Endpoints |
| --- | --- |
| Auth | `POST /api/v1/auth/login`, `login/2fa`, `logout`, `2fa/enroll`, `2fa/activate`, `2fa/disable` |
| Customers | `POST /customers`, `POST /customers/forgot-password`, `POST /customers/reset-password`, `GET /customers/me`, `GET/PUT/DELETE /customers/{id}` |
| Products | `GET /products`, `GET /products/{id}` (cache read-through) |
| Admin products | `POST /admin/products`, `PUT/DELETE /admin/products/{id}` |
| Cart | `GET /cart`, `POST/PUT/DELETE /cart/items`, `DELETE /cart`, `POST /cart/checkout` |
| Orders | `GET /orders`, `GET /orders/{id}` |
| Admin orders | `GET /admin/orders`, `POST /admin/orders/{id}/process` |
| Payments | `GET /payments/charges`, `POST /payments/charges`, `GET /payments/charges/{id}`, `POST /payments/refunds`, `GET /payments/refunds` |
| Admin refunds | `POST /admin/refunds` |
| Shipping | `GET /shipping`, `GET /shipping/track?code=...`, `GET /shipping/orders/{id}` |
| Admin shipments | `POST /admin/shipments`, `PUT /admin/shipments/{id}` |
| Health | `GET /healthz` |

## Quick start

```bash
# infra (Postgres + Redis)
make infra

# run the API locally
make run
# or the whole stack in containers
make infra-app
```

The binary applies the embedded schema and listens on `:8080`. With
`SEED_DEMO=1` (set by `make run` and `docker compose`) it also seeds two demo
accounts and five products. Never set `SEED_DEMO` on a real deployment: the demo
passwords below are public.

| Account | Email | Password |
| --- | --- | --- |
| Admin | `admin@example.test` | `ChangeMe123!` |
| Customer | `customer@example.test` | `ChangeMe123!` |

## Concurrency, demonstrated

1. **Parallel reads (errgroup).** Building `GET /cart` resolves every distinct
   product through the cache-backed store concurrently; `GET /orders/{id}`
   fetches order lines, the payment and the shipment in parallel.
2. **Redis retry/backoff.** Every cache operation (`internal/cache`) retries
   with exponential backoff + jitter. Products degrade to PostgreSQL when Redis
   is down; the app starts with a no-op cache if Redis is unreachable.
3. **Payment restart on cache failure.** `POST /payments/charges` opens with a
   Redis "payment session" marker. A cache failure is treated as transient and
   restarts the payment process (bounded by `MAX_PAYMENT_ATTEMPTS`) — safe
   because every write is keyed by an `Idempotency-Key`, so a restart can never
   double-charge.
4. **Serialised writes.** Stock is reserved with `FOR UPDATE` row locks plus a
   `stock >= 0` CHECK inside the checkout transaction — concurrent checkouts
   cannot oversell. Order status transitions go through the same locking
   discipline with optimistic `version` bumps.

## Idempotency

`Idempotency-Key` is honored on checkout, charges and refunds in two layers:

- an in-memory replay guard (`internal/httpx`) returns the exact cached
  response for a retry, and
- durable `UNIQUE` constraints on `orders/charges/refunds.idempotency_key`
  guarantee exactly-once semantics across restarts and multi-process runs.

## Payments

The provider sits behind the `payment.Gateway` interface with a deterministic
stub. Pass `"simulate": "network"` to see the payment process restart with
backoff and succeed, `"decline"` for a permanent decline, or `"hold"` for an
async `pending` result. Refunds are idempotent partial/full; a fully-refunded
charge flips to `refunded`.

## Configuration

Environment variables (defaults shown):

```
HTTP_ADDR               :8080
DATABASE_URL            postgres://shop:shop@localhost:5432/shop?sslmode=disable
REDIS_ADDR              localhost:6379
REDIS_PASSWORD
JWT_SECRET              dev-secret-change-me
JWT_TTL_MINUTES         60
JWT_CHALLENGE_TTL_MINUTES 5
MAX_PAYMENT_ATTEMPTS    4
SEED_DEMO               (unset)  1 = create the demo accounts and products
DEMO_LOG_RESET_CODES    (unset)  1 = log password-reset codes (no mail relay in the sample)
PAYMENT_RETRY_BASE_DELAY_MS 100
```

## Layout

```
cmd/server/            main + seed
internal/api/          router assembly
internal/{auth,cache}_ auth, jwt, totp, bcrypt / redis + retry/backoff
internal/httpx/        json helpers, middleware, idempotency guard
internal/store/        pgxpool + embedded migrations
internal/{customer,product,cart,order,payment,shipping}/  domain packages
```

## Tests

```bash
go test ./...   # state machines, gateway stub, backoff maths
```

## License

This project is free software, released under the **GNU General Public License v3.0**. You may redistribute and/or modify it under those terms; see [LICENSE.md](LICENSE.md) for the full text.
