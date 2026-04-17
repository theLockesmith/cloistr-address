-- Migration 001: Address Lightning Configuration (UP only)

-- Lightning Address configuration for addresses
CREATE TABLE IF NOT EXISTS address_lightning (
    address_id INTEGER PRIMARY KEY,
    mode VARCHAR(20) NOT NULL DEFAULT 'disabled',
    proxy_address TEXT,
    nwc_connection TEXT,
    min_sendable_msats BIGINT NOT NULL DEFAULT 1000,
    max_sendable_msats BIGINT NOT NULL DEFAULT 100000000000,
    comment_allowed INTEGER NOT NULL DEFAULT 0,
    allows_nostr BOOLEAN NOT NULL DEFAULT true,
    enabled BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT mode_check CHECK (mode IN ('disabled', 'proxy', 'nwc'))
);

CREATE INDEX IF NOT EXISTS idx_address_lightning_address ON address_lightning(address_id);

CREATE OR REPLACE FUNCTION update_address_lightning_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS update_address_lightning_updated_at ON address_lightning;
CREATE TRIGGER update_address_lightning_updated_at
    BEFORE UPDATE ON address_lightning
    FOR EACH ROW EXECUTE FUNCTION update_address_lightning_updated_at();

COMMENT ON TABLE address_lightning IS 'Lightning Address configuration for addresses';
COMMENT ON COLUMN address_lightning.mode IS 'Lightning mode: disabled, proxy (forward to address), or nwc (nostr wallet connect)';
COMMENT ON COLUMN address_lightning.proxy_address IS 'Lightning address to forward payments to (when mode=proxy)';
COMMENT ON COLUMN address_lightning.nwc_connection IS 'NWC connection string (when mode=nwc)';
COMMENT ON COLUMN address_lightning.min_sendable_msats IS 'Minimum sendable amount in millisatoshis';
COMMENT ON COLUMN address_lightning.max_sendable_msats IS 'Maximum sendable amount in millisatoshis';
