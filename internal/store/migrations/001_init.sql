-- 001_init.sql  –  Core schema for the Go shopping‑site sample.
-- Applied at startup via embedded SQL (no external migrate binary needed).

BEGIN;

-- ===== Customers ============================================================
CREATE TABLE IF NOT EXISTS customers (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    full_name     TEXT NOT NULL DEFAULT '',
    is_admin      BOOLEAN NOT NULL DEFAULT FALSE,
    totp_secret   TEXT NOT NULL DEFAULT '',
    totp_enabled  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ          DEFAULT NULL
);
CREATE INDEX IF NOT EXISTS idx_customers_email ON customers (email) WHERE deleted_at IS NULL;

-- ===== Products =============================================================
CREATE TABLE IF NOT EXISTS products (
    id          BIGSERIAL PRIMARY KEY,
    sku         TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    price_cents BIGINT NOT NULL CHECK (price_cents >= 0),
    currency    TEXT NOT NULL DEFAULT 'usd',
    stock       INT  NOT NULL CHECK (stock >= 0) DEFAULT 0,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== Cart =================================================================
CREATE TABLE IF NOT EXISTS cart_items (
    customer_id BIGINT NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    product_id  BIGINT NOT NULL REFERENCES products(id)  ON DELETE CASCADE,
    qty         INT NOT NULL CHECK (qty > 0) DEFAULT 1,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, product_id)
);

-- ===== Orders ===============================================================
CREATE TYPE order_status AS ENUM (
    'pending','paid','processing','shipped','delivered','cancelled'
);

CREATE TABLE IF NOT EXISTS orders (
    id              BIGSERIAL PRIMARY KEY,
    customer_id     BIGINT NOT NULL REFERENCES customers(id),
    status          order_status NOT NULL DEFAULT 'pending',
    total_cents     BIGINT NOT NULL CHECK (total_cents >= 0) DEFAULT 0,
    currency        TEXT NOT NULL DEFAULT 'usd',
    idempotency_key TEXT UNIQUE,
    version         INT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders (customer_id);
CREATE INDEX IF NOT EXISTS idx_orders_status   ON orders (status);

CREATE TABLE IF NOT EXISTS order_items (
    order_id        BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    product_id      BIGINT NOT NULL REFERENCES products(id),
    product_name    TEXT NOT NULL,
    unit_price_cents BIGINT NOT NULL CHECK (unit_price_cents >= 0),
    qty             INT NOT NULL CHECK (qty > 0),
    PRIMARY KEY (order_id, product_id)
);

-- ===== Payments / Charges ===================================================
CREATE TYPE charge_status AS ENUM ('pending','succeeded','failed','refunded');

CREATE TABLE IF NOT EXISTS charges (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id            BIGINT UNIQUE NOT NULL REFERENCES orders(id),
    customer_id         BIGINT NOT NULL REFERENCES customers(id),
    amount_cents        BIGINT NOT NULL CHECK (amount_cents >= 0),
    currency            TEXT NOT NULL DEFAULT 'usd',
    status              charge_status NOT NULL DEFAULT 'pending',
    idempotency_key     TEXT UNIQUE NOT NULL,
    provider_charge_id  TEXT NOT NULL DEFAULT '',
    failure_reason      TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== Refunds ==============================================================
CREATE TYPE refund_status AS ENUM ('pending','succeeded','failed');

CREATE TABLE IF NOT EXISTS refunds (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    charge_id            UUID NOT NULL REFERENCES charges(id),
    customer_id          BIGINT NOT NULL REFERENCES customers(id),
    amount_cents         BIGINT NOT NULL CHECK (amount_cents > 0),
    status               refund_status NOT NULL DEFAULT 'pending',
    idempotency_key      TEXT UNIQUE NOT NULL,
    provider_refund_id   TEXT NOT NULL DEFAULT '',
    failure_reason       TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== Shipments ============================================================
CREATE TYPE shipment_status AS ENUM ('pending','picked','in_transit','delivered');

CREATE TABLE IF NOT EXISTS shipments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id     BIGINT UNIQUE NOT NULL REFERENCES orders(id),
    tracking_code TEXT UNIQUE NOT NULL,
    courier      TEXT NOT NULL DEFAULT '',
    status       shipment_status NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_shipments_tracking ON shipments (tracking_code);

COMMIT;
