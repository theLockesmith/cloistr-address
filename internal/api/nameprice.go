package api

import (
	"context"
	"errors"
	"log/slog"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// AdditionalAddressProductID prices a second or later free-tier name.
const AdditionalAddressProductID = "address_standard_additional"

// fallbackAdditionalPriceSats is used when the catalog row is missing.
//
// Deliberately NOT zero. Zero is the free price, so falling back to it would
// silently restore exactly the behaviour this rule exists to stop — an
// unconfigured catalog would hand out unlimited free names and look like it was
// working. 1000 matches the seeded product and docs/pricing-purchase-flows.md.
const fallbackAdditionalPriceSats int64 = 1000

// NamePrice is what a specific pubkey would pay for a specific name.
type NamePrice struct {
	// PriceSats is what THIS pubkey pays. Zero means genuinely free.
	PriceSats int64
	// Tier is the length-based tier name (standard, short, premium, ultra).
	Tier string
	// Additional is true when the length-based price was free but this account
	// has already claimed its one free name, so the additional-name price
	// applies. Lets the UI explain the charge instead of just showing a number.
	Additional bool
}

// priceNameFor resolves the price of `username` for `pubkey`.
//
// One free real name per account: the length tiers stay as they are (6+ is
// free, short/premium/ultra are paid), but a free-tier name stops being free
// once the account already holds a claimed one. Paid tiers are unaffected —
// they were never the free allowance.
//
// An empty pubkey means "nobody in particular is asking" (the public
// availability endpoint), and gets the length-based price. That is the honest
// answer for an anonymous caller: it is what a new account would pay.
//
// A lookup failure is an error, never a silent zero. Reporting a name as free
// when we could not determine the price is what sent users to a "Claim Free"
// button that could not work.
func (h *Handler) priceNameFor(ctx context.Context, username, pubkey string) (NamePrice, error) {
	base, err := h.store.GetUsernamePrice(ctx, len(username))
	if err != nil {
		return NamePrice{}, err
	}
	tier, err := h.store.GetUsernameTier(ctx, len(username))
	if err != nil {
		return NamePrice{}, err
	}

	price := NamePrice{PriceSats: base, Tier: tier}

	// Only the free tier has an allowance to use up.
	if base != 0 || pubkey == "" {
		return price, nil
	}

	claimed, err := h.store.CountRealAddresses(ctx, pubkey)
	if err != nil {
		return NamePrice{}, err
	}
	if claimed == 0 {
		return price, nil
	}

	additional, err := h.store.GetProductPriceSats(ctx, AdditionalAddressProductID)
	if err != nil {
		if !errors.Is(err, storage.ErrProductNotFound) {
			return NamePrice{}, err
		}
		// Missing catalog row: charge the documented price rather than zero, and
		// say so loudly. Free is the one answer that cannot be safely guessed.
		slog.Error("additional-address product missing from catalog; using fallback price",
			"product_id", AdditionalAddressProductID, "fallback_sats", fallbackAdditionalPriceSats)
		additional = fallbackAdditionalPriceSats
	}

	price.PriceSats = additional
	price.Additional = true
	return price, nil
}
