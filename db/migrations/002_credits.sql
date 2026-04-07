-- Migration: Add credits system for race-based purchases
-- When payment wins but username is taken, user gets withdrawable credits

-- +migrate Up

-- Add last_transfer_at to addresses table for transfer cooldown tracking
ALTER TABLE addresses ADD COLUMN IF NOT EXISTS last_transfer_at TIMESTAMPTZ;

-- Credits: store prepaid credits for users (refunds when username taken)
CREATE TABLE IF NOT EXISTS pubkey_credits (
    pubkey VARCHAR(64) PRIMARY KEY,
    balance_sats BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT pubkey_format CHECK (pubkey ~ '^[a-f0-9]{64}$'),
    CONSTRAINT balance_non_negative CHECK (balance_sats >= 0)
);

-- Credit history for audit trail
CREATE TABLE IF NOT EXISTS credit_history (
    id SERIAL PRIMARY KEY,
    pubkey VARCHAR(64) NOT NULL,
    amount_sats INTEGER NOT NULL,             -- Positive = credit, negative = debit
    reason TEXT NOT NULL,                     -- e.g., 'username_taken', 'withdrawal', 'purchase_applied'
    reference_id TEXT,                        -- e.g., invoice_id or username or Lightning payment hash
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_credit_history_pubkey ON credit_history(pubkey);
CREATE INDEX IF NOT EXISTS idx_credit_history_created ON credit_history(created_at);

-- Withdrawal requests: tracks pending credit withdrawals
CREATE TABLE IF NOT EXISTS credit_withdrawals (
    id SERIAL PRIMARY KEY,
    pubkey VARCHAR(64) NOT NULL,
    amount_sats BIGINT NOT NULL,
    lightning_address TEXT NOT NULL,          -- Where to pay (user's LN address)
    status VARCHAR(20) NOT NULL DEFAULT 'pending',  -- pending, processing, completed, failed
    payment_hash TEXT,                        -- Our outgoing payment hash
    error_message TEXT,                       -- If failed, why
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,

    CONSTRAINT status_check CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    CONSTRAINT amount_positive CHECK (amount_sats > 0)
);

CREATE INDEX IF NOT EXISTS idx_credit_withdrawals_pubkey ON credit_withdrawals(pubkey);
CREATE INDEX IF NOT EXISTS idx_credit_withdrawals_status ON credit_withdrawals(status);

-- Trigger to update pubkey_credits.updated_at
CREATE OR REPLACE FUNCTION update_pubkey_credits_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER update_pubkey_credits_updated_at
    BEFORE UPDATE ON pubkey_credits
    FOR EACH ROW EXECUTE FUNCTION update_pubkey_credits_updated_at();

-- Comments
COMMENT ON TABLE pubkey_credits IS 'Withdrawable credits per pubkey (race loss refunds, overpayments)';
COMMENT ON TABLE credit_history IS 'Audit trail for credit additions and deductions';
COMMENT ON TABLE credit_withdrawals IS 'Tracks credit withdrawal requests to Lightning addresses';

-- +migrate Down

DROP TRIGGER IF EXISTS update_pubkey_credits_updated_at ON pubkey_credits;
DROP FUNCTION IF EXISTS update_pubkey_credits_updated_at();
DROP TABLE IF EXISTS credit_withdrawals;
DROP TABLE IF EXISTS credit_history;
DROP TABLE IF EXISTS pubkey_credits;
ALTER TABLE addresses DROP COLUMN IF EXISTS last_transfer_at;
