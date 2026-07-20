-- Migration 007: enforcement spine — identity-scaled quota resolution
--
-- Wires the pricing model (arbiter/cloistr/architecture/pricing-model.md) into the DB:
--   * addresses.auto_assigned            — an auto-assigned adjective-noun-NNNN address does
--                                          NOT confer the 1 GiB "named" tier (sybil control).
--   * quota_types.storage_bytes free tier drops 1 GiB -> 100 MB for anonymous identities.
--   * effective_quota() / check_quota()  — authoritative quota resolution: tier default (or
--                                          per-user override) + non-expired grants, minus the
--                                          SUM of per-service usage in user_quota_usage.
--   * get_user_tier()                    — anonymous | named | paid, for tier-aware services.
--
-- OWNERSHIP: runs as DB user `cloistr` (run-migrations init container, no ON_ERROR_STOP, no
-- tracking table). Therefore idempotent, NO transaction wrapper (005 lesson: a failing stmt
-- inside a txn silently rolls back the whole file), and touches ONLY cloistr-owned objects:
--   - addresses, address_ownership     -> cloistr-owned (safe to ALTER / index)
--   - quota_types, user_quotas         -> DML only (config tables; cloistr may UPDATE)
--   - effective_quota/check_quota/get_user_tier are NEW functions -> created by cloistr, owned
--     by cloistr (CREATE OR REPLACE is safe on re-run).
--   - quota_grants, user_quota_usage   -> SELECTed only here (reading needs no ownership). The
--     idx_quota_grants_active_lookup index below needs cloistr to OWN quota_grants; a one-time
--     `ALTER TABLE quota_grants OWNER TO cloistr; ALTER TABLE user_quota_usage OWNER TO cloistr;`
--     is applied as postgres alongside this deploy (also recorded in unified-platform-schema.sql).
--     If that ALTER has not run yet, only the two index statements no-op-fail (must be owner) and
--     the rest of the migration still applies cleanly — they succeed on the next pod start.
--   - get_user_quota()/has_service_access() are postgres-owned and are intentionally NOT touched;
--     the Go read path (cloistr-common/platform) calls effective_quota() directly.

-- 1. Identity: mark auto-assigned addresses (does not grant the named tier) -----------
ALTER TABLE addresses ADD COLUMN IF NOT EXISTS auto_assigned BOOLEAN NOT NULL DEFAULT FALSE;

-- 2. Anonymous free-tier storage floor: 100 MB (named identities keep default_limit = 1 GiB) --
UPDATE quota_types SET default_limit_free = 104857600
WHERE id = 'storage_bytes' AND default_limit_free <> 104857600;

-- 3. effective_quota(): authoritative limit/used/remaining for a pubkey + quota type ---------
--    limit = base(tier default OR per-user override) + SUM(non-expired grants)
--    used  = SUM(user_quota_usage across all services)
--    A named identity = has >=1 active address that is NOT auto-assigned.
--    limit = 0 means unlimited (matches the platform Go layer's QuotaInfo.Unlimited()); in that
--    case remaining is reported as 0 and callers treat it as unlimited.
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
BEGIN
    is_named := EXISTS (
        SELECT 1 FROM addresses
        WHERE pubkey = check_pubkey
          AND active = TRUE
          AND COALESCE(auto_assigned, FALSE) = FALSE
    );

    -- Per-user override wins over the tier default when present.
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

        -- Unknown quota type -> unlimited (no configured limit).
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

    -- Unlimited base (0) stays unlimited regardless of grants.
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

-- 4. check_quota(): can this pubkey absorb `additional_bytes` more of this quota type? --------
CREATE OR REPLACE FUNCTION check_quota(check_pubkey CHAR(64), check_quota_type VARCHAR(50), additional_bytes BIGINT)
RETURNS BOOLEAN AS $$
DECLARE
    eq RECORD;
BEGIN
    SELECT * INTO eq FROM effective_quota(check_pubkey, check_quota_type);
    IF eq.quota_limit = 0 THEN
        RETURN TRUE;  -- unlimited
    END IF;
    RETURN (eq.current_usage + GREATEST(0, additional_bytes)) <= eq.quota_limit;
END;
$$ LANGUAGE plpgsql;

-- 5. get_user_tier(): identity state for tier-aware services ----------------------------------
--    paid      = has a non-expired quota grant OR an active subscription
--    named     = has an active, non-auto-assigned address
--    anonymous = everything else (extension-only / auto-assigned only)
CREATE OR REPLACE FUNCTION get_user_tier(check_pubkey CHAR(64))
RETURNS TEXT AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM quota_grants qg
        WHERE qg.pubkey = check_pubkey
          AND (qg.expires_at IS NULL OR qg.expires_at > NOW())
    ) OR EXISTS (
        SELECT 1 FROM subscriptions s
        WHERE s.pubkey = check_pubkey AND s.status = 'active'
    ) THEN
        RETURN 'paid';
    END IF;

    IF EXISTS (
        SELECT 1 FROM addresses
        WHERE pubkey = check_pubkey
          AND active = TRUE
          AND COALESCE(auto_assigned, FALSE) = FALSE
    ) THEN
        RETURN 'named';
    END IF;

    RETURN 'anonymous';
END;
$$ LANGUAGE plpgsql;

-- 6. Indexes -------------------------------------------------------------------------
-- Current-owner / NIP-05 lookup by name (address_ownership is cloistr-owned; safe).
CREATE INDEX IF NOT EXISTS idx_addr_own_current
    ON address_ownership(username, domain) WHERE valid_to IS NULL;

-- Active-grant lookup for effective_quota. quota_grants must be cloistr-owned for this to
-- create; if not yet re-owned (see OWNERSHIP note) this line no-op-fails and the rest applies.
CREATE INDEX IF NOT EXISTS idx_quota_grants_active_lookup
    ON quota_grants(pubkey, quota_type_id, expires_at);
