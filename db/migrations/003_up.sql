-- Migration 003: Add display_name to addresses table
-- This column is used by cloistr-email for email display names

ALTER TABLE addresses ADD COLUMN IF NOT EXISTS display_name VARCHAR(255);

COMMENT ON COLUMN addresses.display_name IS 'Display name for email headers (e.g., "Alice Smith" in "Alice Smith <alice@cloistr.xyz>")';
