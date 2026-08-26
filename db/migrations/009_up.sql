-- Migration 009: record the storage top-up grant window (REWRITTEN 2026-08-26)
--
-- The first version of this file set products.billing_period = '30d' and FAILED
-- on every pod start:
--
--   ERROR: new row for relation "products" violates check constraint
--          "billing_period_check"
--
-- The constraint is structural, not a value whitelist:
--   CHECK ((product_type = 'subscription' AND billing_period IS NOT NULL)
--       OR (product_type = 'one_time'    AND billing_period IS NULL))
--
-- These top-ups are one_time by design — no subscription, no recurring charge —
-- so billing_period CANNOT hold their grant window. That was a wrong concept,
-- not a wrong value, and it never applied in any environment, so rewriting this
-- file in place leaves no environment diverged.
--
-- It also went unnoticed because the runner has no ON_ERROR_STOP (deliberate,
-- see 007's header): it printed the ERROR and then "Migrations complete."
--
-- The window now lives in grants_quota_increases, the jsonb that already
-- describes what buying the product grants. That keeps price, payload and
-- duration in the single row that defines the offer, which is the whole point:
-- a commercial term should not live in Go, or in a display name.
--
-- `expires_days` is a RESERVED key in that payload and is NOT a quota type. The
-- settlement path skips it when creating grants (see internal/api/settlement.go).
--
-- OWNERSHIP: runs as DB user `cloistr`. DML only against the cloistr-owned
-- `products` config table. Idempotent, no transaction wrapper (005 lesson).

UPDATE products
SET grants_quota_increases = grants_quota_increases || '{"expires_days": 30}'::jsonb
WHERE id IN ('storage_topup_10', 'storage_topup_50', 'storage_topup_100')
  AND COALESCE(grants_quota_increases->>'expires_days', '') <> '30';
