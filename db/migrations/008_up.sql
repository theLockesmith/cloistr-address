-- Migration 008: one free name per account
--
-- The free tier prices by LENGTH only: username_tiers.standard is 0 sats for 6+
-- characters, and get_username_price() takes a length and nothing else. Nothing
-- anywhere counts how many names a pubkey already holds, so a single key could
-- claim unlimited free 6+ names and squatting the namespace cost nothing.
--
-- The intended policy (operator, 2026-08-25, and docs/pricing-purchase-flows.md
-- "First 5+ char address is FREE (one per account)") is one free real name per
-- account, with additional names charged. This adds the price for those; the
-- counting rule lives in the Go purchase path, which is where the authenticated
-- pubkey is known.
--
-- "Real" means auto_assigned = FALSE. Every authenticated pubkey is handed an
-- adjective-noun-NNNN auto address by the NIP-98 middleware, and that must not
-- consume the free allowance — the same distinction migration 007 introduced for
-- quota tiers and that AtomicRegisterAddress already uses for primary promotion.
--
-- OWNERSHIP: runs as DB user `cloistr`. DML only, against the cloistr-owned
-- `products` config table. Idempotent, no transaction wrapper (005 lesson).

INSERT INTO products (id, display_name, description, price_sats, product_type, billing_period,
                      grants_service_access, grants_quota_increases, bundle_id) VALUES
  ('address_standard_additional',
   'Additional address (6+ chars)',
   'A second or later 6+ character address on the same account. The first is free.',
   1000, 'one_time', NULL, NULL, NULL, NULL)
ON CONFLICT (id) DO UPDATE SET
  display_name = EXCLUDED.display_name,
  description  = EXCLUDED.description,
  price_sats   = EXCLUDED.price_sats,
  product_type = EXCLUDED.product_type;
