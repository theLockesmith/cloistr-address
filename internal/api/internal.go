package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// GrantCreditsRequest represents a request to grant credits to a pubkey
type GrantCreditsRequest struct {
	Pubkey      string `json:"pubkey" binding:"required"`
	AmountSats  int64  `json:"amount_sats" binding:"required,min=1"`
	Reason      string `json:"reason" binding:"required"`
	ReferenceID string `json:"reference_id,omitempty"` // e.g., relay subscription ID
}

// GrantCreditsResponse represents the response to a grant credits request
type GrantCreditsResponse struct {
	Success     bool   `json:"success"`
	Pubkey      string `json:"pubkey"`
	AmountSats  int64  `json:"amount_sats"`
	NewBalance  int64  `json:"new_balance"`
	ReferenceID string `json:"reference_id,omitempty"`
}

// VerifyAddressResponse represents the response to an address verification request
type VerifyAddressResponse struct {
	Valid       bool    `json:"valid"`
	AddressID   int64   `json:"address_id,omitempty"`
	Username    string  `json:"username,omitempty"`
	Pubkey      string  `json:"pubkey,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
	Active      bool    `json:"active,omitempty"`
}

// internalAuthMiddleware validates the internal API secret
func (h *Handler) internalAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.cfg.InternalAPI.Secret == "" {
			slog.Warn("internal API called but INTERNAL_API_SECRET not configured")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": "Internal API not configured",
			})
			return
		}

		// Check Authorization header
		authHeader := c.GetHeader("Authorization")
		expectedAuth := "Bearer " + h.cfg.InternalAPI.Secret

		if authHeader != expectedAuth {
			slog.Warn("invalid internal API authentication attempt",
				"client_ip", c.ClientIP(),
			)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "Invalid authorization",
			})
			return
		}

		c.Next()
	}
}

// grantCredits grants credits to a pubkey (internal API for service-to-service calls)
// POST /internal/v1/credits/grant
func (h *Handler) grantCredits(c *gin.Context) {
	ctx := c.Request.Context()

	var req GrantCreditsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Validate pubkey format (64 hex chars)
	if len(req.Pubkey) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}

	// Validate reason
	validReasons := map[string]bool{
		"relay_bundle":        true, // Relay subscription includes free address credits
		"relay_upgrade":       true, // Upgrade credit from existing NIP-05
		"promotional":         true, // Promotional credits
		"admin_grant":         true, // Admin-granted credits
		"referral":            true, // Referral program credits
	}
	if !validReasons[req.Reason] {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         "Invalid reason",
			"valid_reasons": []string{"relay_bundle", "relay_upgrade", "promotional", "admin_grant", "referral"},
		})
		return
	}

	// Add credits
	err := h.store.AddCredits(ctx, req.Pubkey, req.AmountSats, req.Reason, req.ReferenceID)
	if err != nil {
		slog.Error("failed to grant credits",
			"pubkey", req.Pubkey,
			"amount_sats", req.AmountSats,
			"reason", req.Reason,
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to grant credits"})
		return
	}

	// Get new balance
	newBalance, err := h.store.GetCredits(ctx, req.Pubkey)
	if err != nil {
		slog.Warn("failed to get new balance after grant", "pubkey", req.Pubkey, "error", err)
	}

	slog.Info("credits granted",
		"pubkey", req.Pubkey,
		"amount_sats", req.AmountSats,
		"reason", req.Reason,
		"reference_id", req.ReferenceID,
		"new_balance", newBalance,
	)

	c.JSON(http.StatusOK, GrantCreditsResponse{
		Success:     true,
		Pubkey:      req.Pubkey,
		AmountSats:  req.AmountSats,
		NewBalance:  newBalance,
		ReferenceID: req.ReferenceID,
	})
}

// verifyAddress verifies that a pubkey owns a specific username
// GET /internal/v1/addresses/verify?username=X&pubkey=Y
// Used by cloistr-email to verify address ownership before allowing email sending
func (h *Handler) verifyAddress(c *gin.Context) {
	ctx := c.Request.Context()

	username := c.Query("username")
	pubkey := c.Query("pubkey")

	// Validate required parameters
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username parameter required"})
		return
	}
	if pubkey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pubkey parameter required"})
		return
	}

	// Validate pubkey format (64 hex chars)
	if len(pubkey) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}

	// Look up the address by username
	address, err := h.store.GetAddressByUsername(ctx, username, h.cfg.Domain)
	if err != nil {
		slog.Debug("address lookup failed",
			"username", username,
			"error", err,
		)
		c.JSON(http.StatusOK, VerifyAddressResponse{Valid: false})
		return
	}

	// Check if the pubkey matches
	if address.Pubkey != pubkey {
		slog.Debug("pubkey mismatch for address verification",
			"username", username,
			"expected_pubkey", address.Pubkey[:8]+"...",
			"provided_pubkey", pubkey[:8]+"...",
		)
		c.JSON(http.StatusOK, VerifyAddressResponse{Valid: false})
		return
	}

	// Check if the address is active
	if !address.Active {
		slog.Debug("address not active",
			"username", username,
		)
		c.JSON(http.StatusOK, VerifyAddressResponse{
			Valid:  false,
			Active: false,
		})
		return
	}

	slog.Info("address verified",
		"username", username,
		"pubkey", pubkey[:8]+"...",
		"address_id", address.ID,
	)

	c.JSON(http.StatusOK, VerifyAddressResponse{
		Valid:       true,
		AddressID:   address.ID,
		Username:    address.Username,
		Pubkey:      address.Pubkey,
		DisplayName: address.DisplayName,
		Active:      address.Active,
	})
}
