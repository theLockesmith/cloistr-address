package api

import (
	"log/slog"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
)

// UsernameAvailabilityResponse represents username availability check result
type UsernameAvailabilityResponse struct {
	Username  string `json:"username"`
	Available bool   `json:"available"`
	PriceSats int64  `json:"price_sats,omitempty"`
	Tier      string `json:"tier,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// AddressResponse represents a user's address details
type AddressResponse struct {
	Username   string                    `json:"username"`
	Domain     string                    `json:"domain"`
	Pubkey     string                    `json:"pubkey"`
	Active     bool                      `json:"active"`
	Verified   bool                      `json:"verified"`
	Lightning  *LightningConfigResponse  `json:"lightning,omitempty"`
	Relays     []string                  `json:"relays,omitempty"`
}

// LightningConfigResponse represents Lightning Address config
type LightningConfigResponse struct {
	Mode           string  `json:"mode"`
	ProxyAddress   string  `json:"proxy_address,omitempty"`
	MinSendableSat int64   `json:"min_sendable_sat"`
	MaxSendableSat int64   `json:"max_sendable_sat"`
	CommentAllowed int     `json:"comment_allowed"`
	AllowsNostr    bool    `json:"allows_nostr"`
	Enabled        bool    `json:"enabled"`
}

// UpdateLightningConfigRequest represents a request to update Lightning config
type UpdateLightningConfigRequest struct {
	Mode         string  `json:"mode" binding:"required,oneof=proxy nwc disabled"`
	ProxyAddress *string `json:"proxy_address,omitempty"`
}

var usernameRegex = regexp.MustCompile(`^[a-z0-9_-]{2,50}$`)

// checkUsernameAvailability checks if a username is available
// GET /api/v1/addresses/check/:username
func (h *Handler) checkUsernameAvailability(c *gin.Context) {
	username := c.Param("username")
	ctx := c.Request.Context()

	// Validate username format
	if !usernameRegex.MatchString(username) {
		c.JSON(http.StatusOK, UsernameAvailabilityResponse{
			Username:  username,
			Available: false,
			Reason:    "Invalid username format. Must be 2-50 characters, lowercase letters, numbers, underscore, or hyphen.",
		})
		return
	}

	// Check availability
	available, err := h.store.IsUsernameAvailable(ctx, username)
	if err != nil {
		slog.Error("failed to check username availability", "username", username, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	response := UsernameAvailabilityResponse{
		Username:  username,
		Available: available,
	}

	if available {
		// Get pricing info
		price, err := h.store.GetUsernamePrice(ctx, len(username))
		if err != nil {
			slog.Warn("failed to get username price", "username", username, "error", err)
		} else {
			response.PriceSats = price
		}

		tier, err := h.store.GetUsernameTier(ctx, len(username))
		if err != nil {
			slog.Warn("failed to get username tier", "username", username, "error", err)
		} else {
			response.Tier = tier
		}
	} else {
		response.Reason = "Username is not available"
	}

	c.JSON(http.StatusOK, response)
}

// getMyAddress returns the authenticated user's address
// GET /api/v1/addresses/me
// TODO: Requires NIP-98 authentication middleware
func (h *Handler) getMyAddress(c *gin.Context) {
	// TODO: Extract pubkey from NIP-98 auth header
	// For now, return not implemented
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": "Authentication not yet implemented",
	})
}

// updateLightningConfig updates the user's Lightning Address configuration
// PUT /api/v1/addresses/lightning
// TODO: Requires NIP-98 authentication middleware
func (h *Handler) updateLightningConfig(c *gin.Context) {
	// TODO: Implement after NIP-98 auth
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": "Authentication not yet implemented",
	})
}
