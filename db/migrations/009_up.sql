-- Migration 009: make the storage top-up grant window explicit
--
-- storage_topup_10/50/100 are named "(30d)" and described as "for 30 days", but
-- their billing_period is NULL because they are product_type = one_time. The
-- settlement path has to write quota_grants.expires_at, and reading the duration
-- out of a display name — or hardcoding 30 days in Go — puts the commercial term
-- somewhere nobody looks when they change the offer.
--
-- Recording it as data means the catalog row is the single place the offer is
-- defined: price, quota payload, AND duration.
--
-- OWNERSHIP: runs as DB user `cloistr`. DML only against the cloistr-owned
-- `products` config table. Idempotent, no transaction wrapper (005 lesson).

UPDATE products SET billing_period = '30d'
WHERE id IN ('storage_topup_10', 'storage_topup_50', 'storage_topup_100')
  AND billing_period IS DISTINCT FROM '30d';
