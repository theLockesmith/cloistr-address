-- Migration 002: Credits system (UP only)

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
    amount_sats INTEGER NOT NULL,
    reason TEXT NOT NULL,
    reference_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_credit_history_pubkey ON credit_history(pubkey);
CREATE INDEX IF NOT EXISTS idx_credit_history_created ON credit_history(created_at);

-- Withdrawal requests: tracks pending credit withdrawals
CREATE TABLE IF NOT EXISTS credit_withdrawals (
    id SERIAL PRIMARY KEY,
    pubkey VARCHAR(64) NOT NULL,
    amount_sats BIGINT NOT NULL,
    lightning_address TEXT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    payment_hash TEXT,
    error_message TEXT,
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

DROP TRIGGER IF EXISTS update_pubkey_credits_updated_at ON pubkey_credits;
CREATE TRIGGER update_pubkey_credits_updated_at
    BEFORE UPDATE ON pubkey_credits
    FOR EACH ROW EXECUTE FUNCTION update_pubkey_credits_updated_at();

COMMENT ON TABLE pubkey_credits IS 'Withdrawable credits per pubkey (race loss refunds, overpayments)';
COMMENT ON TABLE credit_history IS 'Audit trail for credit additions and deductions';
COMMENT ON TABLE credit_withdrawals IS 'Tracks credit withdrawal requests to Lightning addresses';
