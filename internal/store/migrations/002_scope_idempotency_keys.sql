-- 002_scope_idempotency_keys.sql
-- Idempotency keys are chosen by the client, so they must be unique per customer, not
-- globally: with a global UNIQUE, a key one customer had used made another customer's
-- request collide with it (and the replay lookups returned the other customer's row).

ALTER TABLE orders  DROP CONSTRAINT IF EXISTS orders_idempotency_key_key;
ALTER TABLE charges DROP CONSTRAINT IF EXISTS charges_idempotency_key_key;
ALTER TABLE refunds DROP CONSTRAINT IF EXISTS refunds_idempotency_key_key;

ALTER TABLE orders  ADD CONSTRAINT orders_customer_idempotency_key  UNIQUE (customer_id, idempotency_key);
ALTER TABLE charges ADD CONSTRAINT charges_customer_idempotency_key UNIQUE (customer_id, idempotency_key);
ALTER TABLE refunds ADD CONSTRAINT refunds_customer_idempotency_key UNIQUE (customer_id, idempotency_key);
