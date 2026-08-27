package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// PricingTier is one length-based price, as the signup page should display it.
type PricingTier struct {
	Tier      string `json:"tier"`       // database id: standard, short, premium, ultra_premium
	MinLength int    `json:"min_length"` // inclusive
	MaxLength *int   `json:"max_length"` // inclusive; null means "and up"
	PriceSats int64  `json:"price_sats"` // 0 is a REAL price — the free tier
}

// PricingTiersResponse is the whole public price list.
type PricingTiersResponse struct {
	Tiers []PricingTier `json:"tiers"`
	// AdditionalAddressSats is what a SECOND or later free-tier name costs.
	// Without it the free tier is only half the truth: the first 6+ name is
	// free, the next one is not, and a table showing only "Free" would be as
	// wrong as the hardcoded one it replaces.
	AdditionalAddressSats int64 `json:"additional_address_sats"`
}

// listPricingTiers serves the public price list.
//
// # WHY THIS EXISTS
//
// me-ui's "Simple Pricing" table was four blocks of hardcoded JSX. It claimed
// a 6+ character name costs 1,000 sats. Migration 006 set BOTH
// products.address_standard and username_tiers.standard to 0 — the first 6+
// name is FREE, and 1,000 sats is address_standard_additional, i.e. a SECOND
// name. So the signup page told every new visitor their first address costs
// money when it did not.
//
// That is the same class of bug as the availability check being priced
// anonymously: a price rendered from somewhere other than the catalog. The fix
// is the same one — read the catalog. A literal in a component cannot be kept
// in sync with a products table by anything except someone remembering.
//
// Public and unauthenticated on purpose. These are list prices, already
// obtainable one name at a time from /addresses/check/:username, and the
// signup page needs them before anyone has signed in.
//
// A lookup failure is a 500, never a partial list. Half a price table renders
// as a complete one — the missing tier does not leave a gap a reader would
// notice — so serving what we happened to fetch would quietly misprice names.
func (h *Handler) listPricingTiers(c *gin.Context) {
	ctx := c.Request.Context()

	tiers, err := h.store.ListTiers(ctx)
	if err != nil {
		slog.Error("failed to list pricing tiers", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	additional, err := h.additionalAddressPrice(ctx)
	if err != nil {
		slog.Error("failed to read additional-address price", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	// Non-nil so the JSON carries [] rather than null: a client mapping over
	// null throws, and an empty price list is a real state (every tier
	// disabled) that should render as "no tiers" rather than crash.
	out := make([]PricingTier, 0, len(tiers))
	for _, t := range tiers {
		// Disabled tiers are not for sale. Showing them would advertise a price
		// the purchase path will refuse.
		if !t.Enabled {
			continue
		}
		out = append(out, PricingTier{
			Tier:      t.TierName,
			MinLength: t.MinLength,
			MaxLength: t.MaxLength,
			PriceSats: t.PriceSats,
		})
	}

	c.JSON(http.StatusOK, PricingTiersResponse{
		Tiers:                 out,
		AdditionalAddressSats: additional,
	})
}
