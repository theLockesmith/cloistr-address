-- Migration 004: Add NWC fields and hosted mode support
-- Extends address_lightning for Phase 4 NWC integration

-- Add hosted mode to the constraint
ALTER TABLE address_lightning DROP CONSTRAINT IF EXISTS mode_check;
ALTER TABLE address_lightning ADD CONSTRAINT mode_check
    CHECK (mode IN ('proxy', 'nwc', 'hosted', 'disabled'));

-- Add NWC-specific fields for connection management
-- nwc_connection already exists, but we add parsed fields for efficiency
ALTER TABLE address_lightning ADD COLUMN IF NOT EXISTS nwc_relay_url TEXT;
ALTER TABLE address_lightning ADD COLUMN IF NOT EXISTS nwc_wallet_pubkey TEXT;
ALTER TABLE address_lightning ADD COLUMN IF NOT EXISTS nwc_secret_encrypted TEXT;

-- Track NWC connection health
ALTER TABLE address_lightning ADD COLUMN IF NOT EXISTS nwc_last_success_at TIMESTAMPTZ;
ALTER TABLE address_lightning ADD COLUMN IF NOT EXISTS nwc_last_error TEXT;
ALTER TABLE address_lightning ADD COLUMN IF NOT EXISTS nwc_error_count INTEGER DEFAULT 0;

-- Constraint: NWC mode requires connection details
ALTER TABLE address_lightning DROP CONSTRAINT IF EXISTS nwc_required;
ALTER TABLE address_lightning ADD CONSTRAINT nwc_required CHECK (
    mode != 'nwc' OR (nwc_relay_url IS NOT NULL AND nwc_wallet_pubkey IS NOT NULL AND nwc_secret_encrypted IS NOT NULL)
);

-- Comments
COMMENT ON COLUMN address_lightning.nwc_relay_url IS 'NWC relay URL for wallet communication';
COMMENT ON COLUMN address_lightning.nwc_wallet_pubkey IS 'NWC wallet service pubkey (hex)';
COMMENT ON COLUMN address_lightning.nwc_secret_encrypted IS 'NWC secret encrypted with AES-256-GCM';
COMMENT ON COLUMN address_lightning.nwc_last_success_at IS 'Last successful NWC operation';
COMMENT ON COLUMN address_lightning.nwc_last_error IS 'Last NWC error message';
COMMENT ON COLUMN address_lightning.nwc_error_count IS 'Consecutive NWC errors (reset on success)';
