-- Migration 013: backfill addresses.verified.
--
-- cloistr-me inserted every purchased and auto-assigned address with
-- verified = FALSE and nothing ever set it TRUE, so cloistr-email refused to
-- send from those addresses ("unified address not verified"). The inserts
-- now store TRUE (the address belongs to the pubkey that authenticated or
-- paid for it); this sets TRUE on the rows written before that.
--
-- The init container reapplies every *_up.sql on each pod start, so the
-- backfill is recorded in me_data_migrations and runs only once. A row an
-- operator later sets back to FALSE stays FALSE.

CREATE TABLE IF NOT EXISTS me_data_migrations (
    name TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DO $backfill$
BEGIN
    INSERT INTO me_data_migrations (name) VALUES ('013_addresses_verified_backfill')
    ON CONFLICT (name) DO NOTHING;
    IF FOUND THEN
        UPDATE addresses SET verified = TRUE, updated_at = NOW() WHERE verified = FALSE;
    END IF;
END
$backfill$;
