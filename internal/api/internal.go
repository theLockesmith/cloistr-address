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
