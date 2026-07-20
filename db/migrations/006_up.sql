-- Migration 006: pricing / quota model consolidation (free-tier pivot, 2026-07)
--
-- Captures this session's live changes so fresh installs + self-hosters reproduce them.
-- Runs as DB user `cloistr`, which has CREATE on schema public and INSERT/UPDATE/DELETE on
-- the (postgres-owned) config tables, so it may create its own operational tables and do
-- config DML. It must NOT touch postgres-owned *structure* (the has_service_access() function
-- reorder lives in the base schema, unified-platform-schema.sql).
--
-- Idempotent; NO transaction wrapper (migration 005 owner-drift lesson: a single failing
-- statement must not roll back the rest).

-- 1. Operational tables ------------------------------------------------------------
-- Entitlement ledger: subscription tiers (unused for now), micropayment top-ups, admin grants.
CREATE TABLE IF NOT EXISTS quota_grants (
    id            SERIAL PRIMARY KEY,
    pubkey        CHAR(64)    NOT NULL REFERENCES users(pubkey) ON DELETE CASCADE,
    quota_type_id VARCHAR(50) NOT NULL REFERENCES quota_types(id),
    bytes         BIGINT      NOT NULL,
    source        VARCHAR(20) NOT NULL,            -- subscription | topup | admin
    reference_id  TEXT,
    granted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ,                     -- NULL = perpetual (admin only)
    CONSTRAINT quota_grants_source_check CHECK (source IN ('subscription','topup','admin')),
    CONSTRAINT quota_grants_pubkey_hex   CHECK (pubkey ~ '^[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS idx_quota_grants_lookup  ON quota_grants(pubkey, quota_type_id);
CREATE INDEX IF NOT EXISTS idx_quota_grants_expires ON quota_grants(expires_at);

-- Per-service usage breakdown; total usage = SUM(bytes). Services push their own component.
CREATE TABLE IF NOT EXISTS user_quota_usage (
    pubkey        CHAR(64)    NOT NULL REFERENCES users(pubkey) ON DELETE CASCADE,
    quota_type_id VARCHAR(50) NOT NULL REFERENCES quota_types(id),
    service       VARCHAR(50) NOT NULL,
    bytes         BIGINT      NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pubkey, quota_type_id, service),
    CONSTRAINT user_quota_usage_not_negative CHECK (bytes >= 0)
);
CREATE INDEX IF NOT EXISTS idx_user_quota_usage_pubkey ON user_quota_usage(pubkey, quota_type_id);

-- 2. Free-tier storage: ONE 1 GiB pool shared across every storage service incl. email --------
UPDATE quota_types SET
    shared_across_services = ARRAY['blossom','drive','photos','documents','vault','tasks','calendar','email'],
    description        = 'Total storage shared across all file services and email (free tier: 1 GiB)',
    default_limit      = 1073741824,
    default_limit_free = 1073741824
WHERE id = 'storage_bytes';

DELETE FROM user_quotas WHERE quota_type_id = 'email_storage_bytes';  -- email shares storage_bytes now
DELETE FROM quota_types WHERE id            = 'email_storage_bytes';

-- 3. Capability is never paywalled: all service access is free (identity namespace stays paid) --
UPDATE services SET tier = 'free'
WHERE id IN ('blossom','drive','email','vault','calendar','chat','contacts','documents','photos','ritual','video');

-- 4. Product catalog: free 6+ name; metered market-rate storage; retire paywalls/subscriptions/perpetual --
DELETE FROM products WHERE id IN (
    'email_unlock','productivity_unlock','security_unlock',   -- capability paywalls
    'storage_plus_25','storage_pro_100','storage_max_500',    -- subscription tiers (rent)
    'storage_10gb','storage_100gb'                            -- perpetual storage (unbounded liability)
);

INSERT INTO products (id, display_name, description, price_sats, product_type, billing_period,
                      grants_service_access, grants_quota_increases, bundle_id) VALUES
  ('storage_topup_10',  'Storage +10 GiB (30d)',  '10 GiB added to your shared pool for 30 days',   500, 'one_time', NULL, NULL, '{"storage_bytes": 10737418240}',  NULL),
  ('storage_topup_50',  'Storage +50 GiB (30d)',  '50 GiB added to your shared pool for 30 days',  2250, 'one_time', NULL, NULL, '{"storage_bytes": 53687091200}',  NULL),
  ('storage_topup_100', 'Storage +100 GiB (30d)', '100 GiB added to your shared pool for 30 days', 4000, 'one_time', NULL, NULL, '{"storage_bytes": 107374182400}', NULL)
ON CONFLICT (id) DO UPDATE SET
  display_name = EXCLUDED.display_name, description = EXCLUDED.description, price_sats = EXCLUDED.price_sats,
  product_type = EXCLUDED.product_type, grants_quota_increases = EXCLUDED.grants_quota_increases;

UPDATE products       SET price_sats = 0, description = 'NIP-05 + LN + Email address (6+ chars, free tier)' WHERE id = 'address_standard';
UPDATE username_tiers SET price_sats = 0, description = 'Standard usernames (6+ chars) - free tier'        WHERE tier_name = 'standard';
