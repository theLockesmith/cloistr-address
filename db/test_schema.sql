-- Test schema matching production (subset needed for integration tests)

CREATE TABLE users (
    pubkey CHAR(64) PRIMARY KEY,
    display_name VARCHAR(255),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    is_platform_admin BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ,
    invited_by CHAR(64),
    notes TEXT
);

CREATE TABLE addresses (
    id SERIAL PRIMARY KEY,
    username VARCHAR(50) NOT NULL,
    domain VARCHAR(255) NOT NULL DEFAULT 'cloistr.xyz',
    pubkey CHAR(64) NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    verified BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ,
    grace_period_ends TIMESTAMPTZ,
    ban_reason TEXT,
    display_name VARCHAR(255),
    last_transfer_at TIMESTAMPTZ,
    is_primary BOOLEAN NOT NULL DEFAULT FALSE,
    nip05_active BOOLEAN NOT NULL DEFAULT FALSE,
    auto_assigned BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE address_lightning (
    address_id INTEGER NOT NULL PRIMARY KEY,
    mode VARCHAR(20) NOT NULL DEFAULT 'disabled',
    proxy_address TEXT,
    nwc_connection TEXT,
    min_sendable_msats BIGINT NOT NULL DEFAULT 1000,
    max_sendable_msats BIGINT NOT NULL DEFAULT 100000000,
    comment_allowed INTEGER NOT NULL DEFAULT 255,
    allows_nostr BOOLEAN NOT NULL DEFAULT TRUE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    nwc_relay_url TEXT,
    nwc_wallet_pubkey TEXT,
    nwc_secret_encrypted TEXT,
    nwc_last_success_at TIMESTAMPTZ,
    nwc_last_error TEXT,
    nwc_error_count INTEGER DEFAULT 0
);

CREATE TABLE reserved_usernames (
    username VARCHAR(50) NOT NULL PRIMARY KEY,
    reserved_for_pubkey CHAR(64),
    reason TEXT,
    reserved_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE address_ownership (
    id SERIAL PRIMARY KEY,
    username VARCHAR(50) NOT NULL,
    domain VARCHAR(255) NOT NULL DEFAULT 'cloistr.xyz',
    pubkey CHAR(64) NOT NULL,
    valid_from TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    valid_to TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE OR REPLACE FUNCTION is_username_available(check_username VARCHAR)
RETURNS BOOLEAN AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM addresses WHERE username = LOWER(check_username) AND active = TRUE) THEN
        RETURN FALSE;
    END IF;
    IF EXISTS (SELECT 1 FROM reserved_usernames WHERE username = LOWER(check_username) AND reserved_for_pubkey IS NULL) THEN
        RETURN FALSE;
    END IF;
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE quota_types (
    id VARCHAR(50) PRIMARY KEY,
    display_name VARCHAR(100) NOT NULL,
    description TEXT,
    unit VARCHAR(20) NOT NULL,
    shared_across_services TEXT[],
    default_limit BIGINT NOT NULL DEFAULT 0,
    default_limit_free BIGINT NOT NULL DEFAULT 0
);

INSERT INTO quota_types (id, display_name, unit, default_limit, default_limit_free)
VALUES ('storage_bytes', 'Storage', 'bytes', 1073741824, 104857600);

CREATE TABLE quota_grants (
    id SERIAL PRIMARY KEY,
    pubkey CHAR(64) NOT NULL,
    quota_type_id VARCHAR(50) NOT NULL,
    bytes BIGINT NOT NULL,
    source VARCHAR(20) NOT NULL,
    reference_id TEXT,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ
);

CREATE TABLE user_quota_usage (
    pubkey CHAR(64) NOT NULL,
    quota_type_id VARCHAR(50) NOT NULL,
    service VARCHAR(50) NOT NULL,
    bytes BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pubkey, quota_type_id, service)
);

CREATE TABLE user_quotas (
    pubkey CHAR(64) NOT NULL,
    quota_type_id VARCHAR(50) NOT NULL,
    quota_limit BIGINT NOT NULL,
    PRIMARY KEY (pubkey, quota_type_id)
);

-- Tenant tables matching PRODUCTION schema
CREATE TABLE tenants (
    id VARCHAR(50) PRIMARY KEY,
    display_name VARCHAR(255) NOT NULL DEFAULT '',
    owner_pubkey CHAR(64) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    billing_pubkey CHAR(64),
    billing_email VARCHAR(255),
    notes TEXT
);

CREATE TABLE tenant_members (
    tenant_id VARCHAR(50) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    pubkey CHAR(64) NOT NULL,
    role VARCHAR(20) NOT NULL DEFAULT 'member',
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    invited_by CHAR(64),
    PRIMARY KEY (tenant_id, pubkey)
);

CREATE TABLE tenant_quotas (
    tenant_id VARCHAR(50) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    quota_type_id VARCHAR(50) NOT NULL,
    quota_limit BIGINT NOT NULL,
    current_usage BIGINT NOT NULL DEFAULT 0,
    last_updated TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, quota_type_id)
);

CREATE INDEX IF NOT EXISTS idx_tenant_members_pubkey ON tenant_members(pubkey);

-- effective_quota function from migration 010
CREATE OR REPLACE FUNCTION effective_quota(check_pubkey CHAR(64), check_quota_type VARCHAR(50))
RETURNS TABLE (quota_limit BIGINT, current_usage BIGINT, remaining BIGINT) AS $$
DECLARE
    is_named BOOLEAN; override BIGINT; base_limit BIGINT;
    grant_total BIGINT; used_total BIGINT; eff_limit BIGINT;
    t_id TEXT; t_limit BIGINT;
BEGIN
    SELECT tm.tenant_id INTO t_id FROM tenant_members tm WHERE tm.pubkey = check_pubkey LIMIT 1;
    IF t_id IS NOT NULL THEN
        SELECT tq.quota_limit INTO t_limit FROM tenant_quotas tq
        WHERE tq.tenant_id = t_id AND tq.quota_type_id = check_quota_type;
        IF t_limit IS NOT NULL THEN
            SELECT COALESCE(SUM(u.bytes), 0) INTO used_total
            FROM user_quota_usage u
            JOIN tenant_members tm ON tm.pubkey = u.pubkey AND tm.tenant_id = t_id
            WHERE u.quota_type_id = check_quota_type;
            SELECT COALESCE(SUM(qg.bytes), 0) INTO grant_total
            FROM quota_grants qg
            JOIN tenant_members tm ON tm.pubkey = qg.pubkey AND tm.tenant_id = t_id
            WHERE qg.quota_type_id = check_quota_type
              AND (qg.expires_at IS NULL OR qg.expires_at > NOW());
            IF t_limit = 0 THEN eff_limit := 0;
            ELSE eff_limit := t_limit + grant_total; END IF;
            RETURN QUERY SELECT eff_limit, used_total,
                CASE WHEN eff_limit = 0 THEN 0::BIGINT ELSE GREATEST(0, eff_limit - used_total) END;
            RETURN;
        END IF;
    END IF;
    is_named := EXISTS (
        SELECT 1 FROM addresses WHERE pubkey = check_pubkey AND active = TRUE
        AND COALESCE(auto_assigned, FALSE) = FALSE
    );
    SELECT uq.quota_limit INTO override FROM user_quotas uq
    WHERE uq.pubkey = check_pubkey AND uq.quota_type_id = check_quota_type;
    IF override IS NOT NULL THEN base_limit := override;
    ELSE
        SELECT CASE WHEN is_named THEN qt.default_limit ELSE qt.default_limit_free END
        INTO base_limit FROM quota_types qt WHERE qt.id = check_quota_type;
        IF base_limit IS NULL THEN base_limit := 0; END IF;
    END IF;
    SELECT COALESCE(SUM(qg.bytes), 0) INTO grant_total FROM quota_grants qg
    WHERE qg.pubkey = check_pubkey AND qg.quota_type_id = check_quota_type
      AND (qg.expires_at IS NULL OR qg.expires_at > NOW());
    SELECT COALESCE(SUM(u.bytes), 0) INTO used_total FROM user_quota_usage u
    WHERE u.pubkey = check_pubkey AND u.quota_type_id = check_quota_type;
    IF base_limit = 0 THEN eff_limit := 0;
    ELSE eff_limit := base_limit + grant_total; END IF;
    RETURN QUERY SELECT eff_limit, used_total,
        CASE WHEN eff_limit = 0 THEN 0::BIGINT ELSE GREATEST(0, eff_limit - used_total) END;
END;
$$ LANGUAGE plpgsql;
