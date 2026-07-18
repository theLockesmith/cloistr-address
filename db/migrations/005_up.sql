-- Migration 005: N-addresses-per-pubkey identity model
--
-- Moves from one address per pubkey to N (one primary + aliases), adds address
-- ownership history (validity intervals) for temporal audit + name->pubkey
-- resolution, and gives audit_log a first-class indexed subject anchor.
--
-- IMPORTANT: the run-migrations init container re-applies every *_up.sql on every
-- pod start with no tracking table, so this file MUST stay fully idempotent.
--
-- OWNERSHIP NOTE: this migration runs as DB user `cloistr`, which owns `addresses`
-- but NOT `public.audit_log` (postgres-owned). It therefore only manages
-- cloistr-owned objects. `audit_log.subject_pubkey` is applied via the base schema
-- as postgres (unified-platform-schema.sql). Do NOT re-add an ALTER audit_log here:
-- it fails "must be owner of table audit_log" and, if wrapped in a transaction with
-- the rest, rolls the whole migration back. No BEGIN/COMMIT wrapper for the same
-- reason — keep each statement autonomous and idempotent.

-- 1. addresses: allow N per pubkey; add primary + NIP-05-active flags -------------
ALTER TABLE addresses DROP CONSTRAINT IF EXISTS addresses_unique_pubkey;
ALTER TABLE addresses ADD COLUMN IF NOT EXISTS is_primary   BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE addresses ADD COLUMN IF NOT EXISTS nip05_active BOOLEAN NOT NULL DEFAULT FALSE;

-- Keep a fast (non-unique) pubkey lookup path now the unique index is gone.
CREATE INDEX IF NOT EXISTS idx_addresses_pubkey ON addresses(pubkey);

-- Enforce at most one primary and one NIP-05-active address per pubkey.
CREATE UNIQUE INDEX IF NOT EXISTS idx_addresses_one_primary
    ON addresses(pubkey) WHERE is_primary;
CREATE UNIQUE INDEX IF NOT EXISTS idx_addresses_one_nip05
    ON addresses(pubkey) WHERE nip05_active;

-- Backfill: promote one existing active address per pubkey to primary + NIP-05-active.
-- Idempotent: only acts for pubkeys that don't already have one.
UPDATE addresses a SET is_primary = TRUE
WHERE NOT EXISTS (SELECT 1 FROM addresses b WHERE b.pubkey = a.pubkey AND b.is_primary)
  AND a.id = (SELECT MIN(c.id) FROM addresses c WHERE c.pubkey = a.pubkey AND c.active);

UPDATE addresses a SET nip05_active = TRUE
WHERE NOT EXISTS (SELECT 1 FROM addresses b WHERE b.pubkey = a.pubkey AND b.nip05_active)
  AND a.id = (SELECT MIN(c.id) FROM addresses c WHERE c.pubkey = a.pubkey AND c.active);

-- 2. address ownership history -----------------------------------------------------
-- Validity intervals per (username, domain) -> pubkey. Backs "who owned this name
-- at time T" for temporal audit search and correct attribution across transfers.
CREATE TABLE IF NOT EXISTS address_ownership (
    id          SERIAL PRIMARY KEY,
    username    VARCHAR(50)  NOT NULL,
    domain      VARCHAR(255) NOT NULL DEFAULT 'cloistr.xyz',
    pubkey      CHAR(64)     NOT NULL,
    valid_from  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    valid_to    TIMESTAMPTZ,            -- NULL = still owned
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT address_ownership_pubkey_hex CHECK (pubkey ~ '^[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS idx_addr_own_name   ON address_ownership(username, domain);
CREATE INDEX IF NOT EXISTS idx_addr_own_pubkey ON address_ownership(pubkey);
CREATE INDEX IF NOT EXISTS idx_addr_own_window ON address_ownership(valid_from, valid_to);

-- Backfill: an open-ended ownership interval for every current active address.
-- Idempotent: skip names that already have an open interval.
INSERT INTO address_ownership (username, domain, pubkey, valid_from)
SELECT a.username, a.domain, a.pubkey, COALESCE(a.created_at, NOW())
FROM addresses a
WHERE a.active
  AND NOT EXISTS (
      SELECT 1 FROM address_ownership o
      WHERE o.username = a.username AND o.domain = a.domain
        AND o.pubkey = a.pubkey AND o.valid_to IS NULL
  );

-- 3. audit_log subject anchor ------------------------------------------------------
-- NOTE: `audit_log.subject_pubkey` (+ idx_audit_log_subject) is required by the audit
-- search/write code but is applied by the base schema as postgres — `audit_log` is
-- postgres-owned and `cloistr` cannot ALTER it. See unified-platform-schema.sql
-- (Migration 005 block). It is intentionally NOT altered here.
