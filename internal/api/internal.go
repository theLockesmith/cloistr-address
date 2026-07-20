package api

import (
	"log/slog"
	"net/http"
	"strconv"

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

// QuotaUsageRequest records a service's usage delta against a pubkey's shared pool.
type QuotaUsageRequest struct {
	Pubkey      string `json:"pubkey" binding:"required"`
	Service     string `json:"service" binding:"required"`
	QuotaTypeID string `json:"quota_type_id"` // defaults to storage_bytes
	Bytes       int64  `json:"bytes"`         // additive delta; negative = release
}

// checkQuota reports whether a pubkey can absorb `bytes` more of a quota type.
// GET /internal/v1/quotas/check?pubkey=&quota_type=&bytes=
// Used by services without a direct DB connection (e.g. a future thin uploader).
func (h *Handler) checkQuota(c *gin.Context) {
	ctx := c.Request.Context()

	pubkey := c.Query("pubkey")
	if len(pubkey) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pubkey query param required (64 hex)"})
		return
	}
	quotaType := c.Query("quota_type")
	if quotaType == "" {
		quotaType = "storage_bytes"
	}
	var bytes int64
	if v := c.Query("bytes"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bytes must be an integer"})
			return
		}
		bytes = parsed
	}

	eq, err := h.store.EffectiveQuota(ctx, pubkey, quotaType)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve quota"})
		return
	}
	allowed, err := h.store.CheckQuota(ctx, pubkey, quotaType, bytes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check quota"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"allowed":   allowed,
		"limit":     eq.Limit,
		"used":      eq.Used,
		"remaining": eq.Remaining,
	})
}

// recordQuotaUsage adds a service's usage delta to a pubkey's shared pool.
// POST /internal/v1/quotas/usage {pubkey, service, quota_type_id, bytes}
func (h *Handler) recordQuotaUsage(c *gin.Context) {
	ctx := c.Request.Context()

	var req QuotaUsageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if len(req.Pubkey) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}
	if req.QuotaTypeID == "" {
		req.QuotaTypeID = "storage_bytes"
	}

	if err := h.store.RecordServiceUsage(ctx, req.Pubkey, req.QuotaTypeID, req.Service, req.Bytes); err != nil {
		slog.Error("failed to record usage",
			"pubkey", req.Pubkey, "service", req.Service, "bytes", req.Bytes, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record usage"})
		return
	}

	eq, err := h.store.EffectiveQuota(ctx, req.Pubkey, req.QuotaTypeID)
	if err != nil {
		// Usage was recorded; failing to read back the total is non-fatal.
		c.JSON(http.StatusOK, gin.H{"success": true})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"limit":     eq.Limit,
		"used":      eq.Used,
		"remaining": eq.Remaining,
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
