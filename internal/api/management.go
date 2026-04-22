package api

import (
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
)

// timeNow is a variable for testing
var timeNow = time.Now

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
func (h *Handler) getMyAddress(c *gin.Context) {
	ctx := c.Request.Context()
	pubkey := auth.GetPubkey(c)

	// Look up address for this pubkey
	address, err := h.store.GetAddressByPubkey(ctx, pubkey)
	if err != nil {
		slog.Error("failed to get address", "pubkey", pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	if address == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "No address registered for this pubkey",
		})
		return
	}

	// Get relay hints
	relays, err := h.store.GetAddressRelays(ctx, address.ID)
	if err != nil {
		slog.Warn("failed to get relays", "address_id", address.ID, "error", err)
	}

	// Get lightning config
	lightning, err := h.store.GetLightningConfig(ctx, address.ID)
	if err != nil {
		slog.Warn("failed to get lightning config", "address_id", address.ID, "error", err)
	}

	response := AddressResponse{
		Username: address.Username,
		Domain:   address.Domain,
		Pubkey:   address.Pubkey,
		Active:   address.Active,
		Verified: address.Verified,
		Relays:   relays,
	}

	if lightning != nil {
		response.Lightning = &LightningConfigResponse{
			Mode:           lightning.Mode,
			ProxyAddress:   lightning.ProxyAddress,
			MinSendableSat: lightning.MinSendableMsats / 1000,
			MaxSendableSat: lightning.MaxSendableMsats / 1000,
			CommentAllowed: lightning.CommentAllowed,
			AllowsNostr:    lightning.AllowsNostr,
			Enabled:        lightning.Enabled,
		}
	}

	c.JSON(http.StatusOK, response)
}

// updateLightningConfig updates the user's Lightning Address configuration
// PUT /api/v1/addresses/lightning
func (h *Handler) updateLightningConfig(c *gin.Context) {
	ctx := c.Request.Context()
	pubkey := auth.GetPubkey(c)

	var req UpdateLightningConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Validate proxy address if mode is proxy
	if req.Mode == "proxy" {
		if req.ProxyAddress == nil || *req.ProxyAddress == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "proxy_address required when mode is 'proxy'"})
			return
		}
		// Basic Lightning Address format validation
		if !isValidLightningAddress(*req.ProxyAddress) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid Lightning Address format"})
			return
		}
	}

	// Get user's address
	address, err := h.store.GetAddressByPubkey(ctx, pubkey)
	if err != nil {
		slog.Error("failed to get address", "pubkey", pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	if address == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No address registered for this pubkey"})
		return
	}

	// Update lightning config
	proxyAddr := ""
	if req.ProxyAddress != nil {
		proxyAddr = *req.ProxyAddress
	}

	err = h.store.UpsertLightningConfig(ctx, address.ID, req.Mode, proxyAddr)
	if err != nil {
		slog.Error("failed to update lightning config", "address_id", address.ID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update configuration"})
		return
	}

	// Fetch updated config
	lightning, err := h.store.GetLightningConfig(ctx, address.ID)
	if err != nil {
		slog.Error("failed to get updated lightning config", "address_id", address.ID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Configuration updated but failed to fetch result"})
		return
	}

	c.JSON(http.StatusOK, LightningConfigResponse{
		Mode:           lightning.Mode,
		ProxyAddress:   lightning.ProxyAddress,
		MinSendableSat: lightning.MinSendableMsats / 1000,
		MaxSendableSat: lightning.MaxSendableMsats / 1000,
		CommentAllowed: lightning.CommentAllowed,
		AllowsNostr:    lightning.AllowsNostr,
		Enabled:        lightning.Enabled,
	})
}

// isValidLightningAddress validates a Lightning Address format (user@domain)
func isValidLightningAddress(addr string) bool {
	parts := strings.Split(addr, "@")
	if len(parts) != 2 {
		return false
	}
	// Basic validation: both parts non-empty
	return len(parts[0]) > 0 && len(parts[1]) > 2 && strings.Contains(parts[1], ".")
}

// transferAddress transfers ownership of an address to another pubkey
// POST /api/v1/addresses/transfer
func (h *Handler) transferAddress(c *gin.Context) {
	ctx := c.Request.Context()
	pubkey := auth.GetPubkey(c)

	var req struct {
		NewPubkey string `json:"new_pubkey" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Validate new pubkey format (64 hex chars)
	if len(req.NewPubkey) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}

	// Can't transfer to self
	if req.NewPubkey == pubkey {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot transfer to yourself"})
		return
	}

	// Get user's address
	address, err := h.store.GetAddressByPubkey(ctx, pubkey)
	if err != nil {
		slog.Error("failed to get address", "pubkey", pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	if address == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No address registered for this pubkey"})
		return
	}

	// Check transfer cooldown (7 days)
	if address.LastTransferAt != nil {
		cooldownEnd := address.LastTransferAt.AddDate(0, 0, 7)
		if cooldownEnd.After(timeNow()) {
			c.JSON(http.StatusTooEarly, gin.H{
				"error":          "Transfer cooldown active",
				"cooldown_ends":  cooldownEnd.Format("2006-01-02T15:04:05Z"),
			})
			return
		}
	}

	// Check if new pubkey already has an address
	existingAddr, err := h.store.GetAddressByPubkey(ctx, req.NewPubkey)
	if err != nil {
		slog.Error("failed to check new pubkey", "new_pubkey", req.NewPubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}
	if existingAddr != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Target pubkey already has an address"})
		return
	}

	// Perform transfer
	err = h.store.TransferAddress(ctx, address.ID, req.NewPubkey)
	if err != nil {
		slog.Error("failed to transfer address", "address_id", address.ID, "new_pubkey", req.NewPubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transfer failed"})
		return
	}

	slog.Info("address transferred", "username", address.Username, "from", pubkey, "to", req.NewPubkey)

	c.JSON(http.StatusOK, gin.H{
		"message":    "Address transferred successfully",
		"username":   address.Username,
		"new_pubkey": req.NewPubkey,
	})
}
