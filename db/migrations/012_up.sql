-- Migration 012: a settled invoice credits its buyer at most once
--
-- The BTCPay webhook credits the buyer when it cannot hand over the name they
-- paid for (race loss, registration error). It now answers 5xx when that
-- credit fails so BTCPay redelivers, and a redelivery must not credit twice.
-- This index is what makes the second INSERT into credit_history a no-op:
-- AddInvoiceCreditOnce inserts the history row first, ON CONFLICT DO NOTHING,
-- and only moves the balance when that insert happened.
--
-- Partial on the webhook's own reasons: other credit reasons reuse
-- reference_id for things that are not invoices. Measured 2026-10-16: no
-- production rows carry these reasons yet, so the index builds without
-- conflict. Idempotent (the init container reapplies every migration).

CREATE UNIQUE INDEX IF NOT EXISTS credit_history_invoice_credit_once
    ON credit_history (reference_id)
    WHERE reason IN ('race_loss', 'registration_error');
