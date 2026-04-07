-- Migration: Add address_lightning table for Lightning Address configuration
-- This extends the unified platform schema with Lightning-specific settings

-- +migrate Up

CREATE TABLE IF NOT EXISTS address_lightning (
    address_id INTEGER PRIMARY KEY REFERENCES addresses(id) ON DELETE CASCADE,

    -- Lightning mode: how payments are handled
    -- 'proxy': forward to external Lightning Address
    -- 'nwc': use Nostr Wallet Connect (future)
    -- 'disabled': Lightning Address not active
    mode VARCHAR(20) NOT NULL DEFAULT 'disabled',

    -- Proxy mode: forward payments to this Lightning Address
    proxy_address TEXT,

    -- NWC mode (future): encrypted connection string
    nwc_connection TEXT,

    -- LNURLP settings
    min_sendable_msats BIGINT NOT NULL DEFAULT 1000,        -- 1 sat minimum
    max_sendable_msats BIGINT NOT NULL DEFAULT 100000000,   -- 100k sats maximum
    comment_allowed INTEGER NOT NULL DEFAULT 255,           -- max comment length

    -- Nostr integration (NIP-57 zaps)
    allows_nostr BOOLEAN NOT NULL DEFAULT TRUE,

    -- Status
    enabled BOOLEAN NOT NULL DEFAULT TRUE,

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Constraints
    CONSTRAINT mode_check CHECK (mode IN ('proxy', 'nwc', 'disabled')),
    CONSTRAINT proxy_required CHECK (
        mode != 'proxy' OR proxy_address IS NOT NULL
    ),
    CONSTRAINT min_max_valid CHECK (min_sendable_msats <= max_sendable_msats),
    CONSTRAINT comment_allowed_valid CHECK (comment_allowed >= 0 AND comment_allowed <= 2000)
);

-- Index for looking up by address
CREATE INDEX IF NOT EXISTS idx_address_lightning_address ON address_lightning(address_id);

-- Trigger to update updated_at
CREATE OR REPLACE FUNCTION update_address_lightning_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER update_address_lightning_updated_at
    BEFORE UPDATE ON address_lightning
    FOR EACH ROW EXECUTE FUNCTION update_address_lightning_updated_at();

-- Comments
COMMENT ON TABLE address_lightning IS 'Lightning Address configuration per user address';
COMMENT ON COLUMN address_lightning.mode IS 'Payment handling mode: proxy (forward), nwc (wallet connect), disabled';
COMMENT ON COLUMN address_lightning.proxy_address IS 'For proxy mode: Lightning Address to forward payments to (e.g., alice@getalby.com)';
COMMENT ON COLUMN address_lightning.nwc_connection IS 'For NWC mode: encrypted Nostr Wallet Connect connection string';
COMMENT ON COLUMN address_lightning.allows_nostr IS 'Whether to include nostrPubkey in LNURLP response for NIP-57 zaps';

-- +migrate Down

DROP TRIGGER IF EXISTS update_address_lightning_updated_at ON address_lightning;
DROP FUNCTION IF EXISTS update_address_lightning_updated_at();
DROP TABLE IF EXISTS address_lightning;
