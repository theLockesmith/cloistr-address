package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// Invoice metadata keys. BTCPay hands these back verbatim on settlement, so they
// are the whole contract between "create an invoice" and "grant the thing".
const (
	MetaKind      = "kind"       // "address" | "product"
	MetaUsername  = "username"   // address purchases
	MetaPubkey    = "pubkey"     // who gets the thing
	MetaProductID = "product_id" // product purchases
)

// QuotaExpiresDaysKey is a RESERVED key inside a product's
// grants_quota_increases payload. It carries the grant window in days and is
// NOT a quota type — settleProduct must skip it when creating grants, or a
// storage top-up would also grant 30 bytes of "expires_days".
//
// It lives in the payload because products.billing_period cannot hold it: the
// table's check constraint requires billing_period IS NULL for one_time
// products, and these top-ups are one_time by design. Migration 009 tried
// billing_period first and failed on every pod start.
const QuotaExpiresDaysKey = "expires_days"

// Settlement kinds.
const (
	KindAddress = "address"
	KindProduct = "product"
)

// SettlementOutcome is what a settled invoice did, for the webhook's response
// and for the log.
type SettlementOutcome struct {
	Status  string
	Message string
	Detail  map[string]any
}

// settlementKind decides what a settled invoice is buying.
//
// # WHY THIS EXISTS
//
// The webhook used to read metadata["username"] and call AtomicRegisterAddress.
// A settled invoice for anything else logged "Missing username" and did nothing
// — which is why storage_topup_10/50/100 have been priced in the catalog since
// migration 006 with no code path able to sell them.
//
// Invoices minted before `kind` existed carry only a username, and BTCPay will
// still deliver them, so an absent kind with a username present means the legacy
// address flow. Guessing any other way would drop a real payment on the floor.
func settlementKind(meta map[string]any) string {
	if k, _ := meta[MetaKind].(string); k != "" {
		return k
	}
	if pid, _ := meta[MetaProductID].(string); pid != "" {
		return KindProduct
	}
	if u, _ := meta[MetaUsername].(string); u != "" {
		return KindAddress
	}
	return ""
}

// grantWindow converts a product's grant window into an expiry.
//
// Returns nil for a permanent grant. An unparseable window is an ERROR, not a
// silent "permanent": quietly granting 100 GiB forever because someone typed
// "thirty" is the expensive direction to be wrong in.
func grantWindow(now time.Time, quotas map[string]int64) (*time.Time, error) {
	days, ok := quotas[QuotaExpiresDaysKey]
	if !ok {
		return nil, nil
	}
	if days <= 0 {
		return nil, fmt.Errorf("%s must be a positive number of days, got %d", QuotaExpiresDaysKey, days)
	}
	t := now.AddDate(0, 0, int(days))
	return &t, nil
}

// settleProduct grants what a catalog product promises.
//
// Idempotent on the invoice ID: BTCPay retries webhooks, and without that check
// a retry doubles the user's storage.
func (h *Handler) settleProduct(ctx context.Context, invoiceID, pubkey, productID string, now time.Time) (SettlementOutcome, error) {
	already, err := h.store.QuotaGrantExists(ctx, invoiceID)
	if err != nil {
		return SettlementOutcome{}, err
	}
	if already {
		slog.Info("settlement already applied, ignoring retry",
			"invoice_id", invoiceID, "product_id", productID)
		return SettlementOutcome{Status: "already_applied", Message: "Grant already recorded"}, nil
	}

	product, err := h.store.GetProduct(ctx, productID)
	if err != nil {
		if errors.Is(err, storage.ErrProductNotFound) {
			// Paid for something the catalog no longer has. Do NOT swallow it:
			// the user is owed either the grant or their sats back.
			return SettlementOutcome{}, fmt.Errorf("settled invoice names unknown product %q: %w", productID, err)
		}
		return SettlementOutcome{}, err
	}
	if len(product.GrantsQuotaIncreases) == 0 {
		return SettlementOutcome{}, fmt.Errorf("product %q grants no quota; nothing to settle", productID)
	}

	expiresAt, err := grantWindow(now, product.GrantsQuotaIncreases)
	if err != nil {
		return SettlementOutcome{}, fmt.Errorf("product %q: %w", productID, err)
	}

	granted := map[string]any{}
	for quotaType, bytes := range product.GrantsQuotaIncreases {
		// Reserved: the window, not a quota. Granting it would hand the user
		// 30 bytes of a quota type called "expires_days".
		if quotaType == QuotaExpiresDaysKey {
			continue
		}
		if err := h.store.AddQuotaGrant(ctx, pubkey, quotaType, bytes, "purchase", invoiceID, expiresAt); err != nil {
			return SettlementOutcome{}, err
		}
		granted[quotaType] = bytes
	}
	if len(granted) == 0 {
		return SettlementOutcome{}, fmt.Errorf("product %q grants only %s and no actual quota", productID, QuotaExpiresDaysKey)
	}

	slog.Info("product settled",
		"invoice_id", invoiceID, "product_id", productID, "pubkey", pubkey,
		"granted", granted, "expires_at", expiresAt)

	return SettlementOutcome{
		Status:  "granted",
		Message: product.DisplayName,
		Detail:  map[string]any{"product_id": productID, "granted": granted},
	}, nil
}
