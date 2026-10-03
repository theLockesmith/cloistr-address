-- Migration 010: tenant quota pooling for fleet storage
--
-- Lets multiple pubkeys pool their quota under a single tenant. The motivating
-- case is fleet storage: the 71 arbiter fleet roles all share one blossom quota
-- instead of 71 separate anonymous-tier allocations.
--
-- Tables:
--   tenants         — a named pool with an owner pubkey
--   tenant_members  — which pubkeys belong to which tenant
--   tenant_quotas   — per-tenant quota limits (replaces individual limits for members)
--
-- effective_quota() is updated: when the checked pubkey is a tenant member,
-- it sums usage across ALL members of that tenant and reads the limit from
-- tenant_quotas (falling back to the individual limit if no tenant quota row).
--
-- OWNERSHIP: runs as DB user `cloistr`. Creates new tables (cloistr-owned) and
-- replaces the effective_quota function (also cloistr-owned since 007). Idempotent,
-- no transaction wrapper (005 lesson).

-- 1. Tenant tables ---------------------------------------------------------------

CREATE TABLE IF NOT EXISTS tenants (
    id           TEXT        PRIMARY KEY,
    owner_pubkey CHAR(64)   NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tenant_members (
    tenant_id TEXT      NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    pubkey    CHAR(64)  NOT NULL,
    added_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, pubkey)
);

CREATE TABLE IF NOT EXISTS tenant_quotas (
    tenant_id     TEXT        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    quota_type_id VARCHAR(50) NOT NULL,
    quota_limit   BIGINT      NOT NULL,
    PRIMARY KEY (tenant_id, quota_type_id)
);

-- Index: look up which tenant a pubkey belongs to (the hot path in effective_quota).
CREATE INDEX IF NOT EXISTS idx_tenant_members_pubkey ON tenant_members(pubkey);

-- 2. Updated effective_quota() with tenant awareness -----------------------------
--
-- When the checked pubkey is a tenant member, the function:
--   a) reads the limit from tenant_quotas (for that tenant + quota type)
--   b) sums usage across ALL members of that tenant
--   c) if no tenant_quotas row exists, falls back to the individual limit
--
-- A pubkey that is NOT a tenant member behaves exactly as before.

CREATE OR REPLACE FUNCTION effective_quota(check_pubkey CHAR(64), check_quota_type VARCHAR(50))
RETURNS TABLE (
    quota_limit   BIGINT,
    current_usage BIGINT,
    remaining     BIGINT
) AS $$
DECLARE
    is_named     BOOLEAN;
    override     BIGINT;
    base_limit   BIGINT;
    grant_total  BIGINT;
    used_total   BIGINT;
    eff_limit    BIGINT;
    t_id         TEXT;
    t_limit      BIGINT;
BEGIN
    -- Check tenant membership first.
    SELECT tm.tenant_id INTO t_id
    FROM tenant_members tm
    WHERE tm.pubkey = check_pubkey
    LIMIT 1;

    IF t_id IS NOT NULL THEN
        -- Tenant path: read the tenant's quota limit for this type.
        SELECT tq.quota_limit INTO t_limit
        FROM tenant_quotas tq
        WHERE tq.tenant_id = t_id AND tq.quota_type_id = check_quota_type;

        IF t_limit IS NOT NULL THEN
            -- Sum usage across all members of this tenant.
            SELECT COALESCE(SUM(u.bytes), 0) INTO used_total
            FROM user_quota_usage u
            JOIN tenant_members tm ON tm.pubkey = u.pubkey AND tm.tenant_id = t_id
            WHERE u.quota_type_id = check_quota_type;

            -- Add any grants held by ANY tenant member.
            SELECT COALESCE(SUM(qg.bytes), 0) INTO grant_total
            FROM quota_grants qg
            JOIN tenant_members tm ON tm.pubkey = qg.pubkey AND tm.tenant_id = t_id
            WHERE qg.quota_type_id = check_quota_type
              AND (qg.expires_at IS NULL OR qg.expires_at > NOW());

            IF t_limit = 0 THEN
                eff_limit := 0;  -- unlimited
            ELSE
                eff_limit := t_limit + grant_total;
            END IF;

            RETURN QUERY SELECT
                eff_limit,
                used_total,
                CASE WHEN eff_limit = 0 THEN 0::BIGINT ELSE GREATEST(0, eff_limit - used_total) END;
            RETURN;
        END IF;
        -- No tenant_quotas row for this type: fall through to individual path.
    END IF;

    -- Individual path (unchanged from migration 007).
    is_named := EXISTS (
        SELECT 1 FROM addresses
        WHERE pubkey = check_pubkey
          AND active = TRUE
          AND COALESCE(auto_assigned, FALSE) = FALSE
    );

    SELECT uq.quota_limit INTO override
    FROM user_quotas uq
    WHERE uq.pubkey = check_pubkey AND uq.quota_type_id = check_quota_type;

    IF override IS NOT NULL THEN
        base_limit := override;
    ELSE
        SELECT CASE WHEN is_named THEN qt.default_limit ELSE qt.default_limit_free END
          INTO base_limit
        FROM quota_types qt
        WHERE qt.id = check_quota_type;

        IF base_limit IS NULL THEN
            base_limit := 0;
        END IF;
    END IF;

    SELECT COALESCE(SUM(qg.bytes), 0) INTO grant_total
    FROM quota_grants qg
    WHERE qg.pubkey = check_pubkey
      AND qg.quota_type_id = check_quota_type
      AND (qg.expires_at IS NULL OR qg.expires_at > NOW());

    SELECT COALESCE(SUM(u.bytes), 0) INTO used_total
    FROM user_quota_usage u
    WHERE u.pubkey = check_pubkey AND u.quota_type_id = check_quota_type;

    IF base_limit = 0 THEN
        eff_limit := 0;
    ELSE
        eff_limit := base_limit + grant_total;
    END IF;

    RETURN QUERY SELECT
        eff_limit,
        used_total,
        CASE WHEN eff_limit = 0 THEN 0::BIGINT ELSE GREATEST(0, eff_limit - used_total) END;
END;
$$ LANGUAGE plpgsql;
