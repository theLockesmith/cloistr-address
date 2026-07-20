package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	nameval "git.aegis-hq.xyz/coldforge/cloistr-common/username"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/nwc"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
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

// AddressEntry is a compact per-address entry in the /addresses/me response.
type AddressEntry struct {
	Username    string    `json:"username"`
	Domain      string    `json:"domain"`
	IsPrimary   bool      `json:"is_primary"`
	NIP05Active bool      `json:"nip05_active"`
	Active      bool      `json:"active"`
	Verified    bool      `json:"verified"`
	DisplayName *string   `json:"display_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// AddressResponse represents a user's address details.
// Top-level fields reflect the primary address for backwards compatibility.
// The Addresses list contains all active addresses for the pubkey.
type AddressResponse struct {
	// Primary-address fields (backward-compatible top-level).
	Username    string  `json:"username"`
	Domain      string  `json:"domain"`
	Pubkey      string  `json:"pubkey"`
	Active      bool    `json:"active"`
	Verified    bool    `json:"verified"`
	IsPrimary   bool    `json:"is_primary"`
	NIP05Active bool    `json:"nip05_active"`
	DisplayName *string `json:"display_name,omitempty"`

	// All active addresses for this pubkey (primary first).
	Addresses []AddressEntry `json:"addresses"`

	Lightning *LightningConfigResponse `json:"lightning,omitempty"`
	Relays    []string                 `json:"relays,omitempty"`
}

// LightningConfigResponse represents Lightning Address config
type LightningConfigResponse struct {
	Mode           string `json:"mode"`
	ProxyAddress   string `json:"proxy_address,omitempty"`
	NWCConfigured  bool   `json:"nwc_configured,omitempty"`
	NWCErrorCount  int    `json:"nwc_error_count,omitempty"`
	MinSendableSat int64  `json:"min_sendable_sat"`
	MaxSendableSat int64  `json:"max_sendable_sat"`
	CommentAllowed int    `json:"comment_allowed"`
	AllowsNostr    bool   `json:"allows_nostr"`
	Enabled        bool   `json:"enabled"`
}

// UpdateLightningConfigRequest represents a request to update Lightning config
type UpdateLightningConfigRequest struct {
	Mode             string  `json:"mode" binding:"required,oneof=proxy nwc hosted disabled"`
	ProxyAddress     *string `json:"proxy_address,omitempty"`
	NWCConnectionURI *string `json:"nwc_connection_uri,omitempty"` // nostr+walletconnect://...
}

// checkUsernameAvailability checks if a username is available
// GET /api/v1/addresses/check/:username
func (h *Handler) checkUsernameAvailability(c *gin.Context) {
	username := c.Param("username")
	ctx := c.Request.Context()

	// Validate username format. IsValidHumanName also rejects the reserved
	// auto-assigned shape (adjective-noun-NNNN) so it can't be squatted.
	if !nameval.IsValidHumanName(username) {
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

// getMyAddress returns the authenticated user's primary address plus their full
// address list.
// GET /api/v1/addresses/me
func (h *Handler) getMyAddress(c *gin.Context) {
	ctx := c.Request.Context()
	pubkey := auth.GetPubkey(c)

	// Look up the primary address.
	address, err := h.store.GetPrimaryAddressByPubkey(ctx, pubkey)
	if err != nil {
		slog.Error("failed to get primary address", "pubkey", pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	if address == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "No address registered for this pubkey",
		})
		return
	}

	// Get relay hints for the primary address.
	relays, err := h.store.GetAddressRelays(ctx, address.ID)
	if err != nil {
		slog.Warn("failed to get relays", "address_id", address.ID, "error", err)
	}

	// Get lightning config for the primary address.
	lightning, err := h.store.GetLightningConfig(ctx, address.ID)
	if err != nil {
		slog.Warn("failed to get lightning config", "address_id", address.ID, "error", err)
	}

	// Get all active addresses for the pubkey.
	allAddrs, err := h.store.GetAddressesByPubkey(ctx, pubkey)
	if err != nil {
		slog.Warn("failed to list addresses", "pubkey", pubkey, "error", err)
	}
	entries := make([]AddressEntry, 0, len(allAddrs))
	for _, a := range allAddrs {
		entries = append(entries, AddressEntry{
			Username:    a.Username,
			Domain:      a.Domain,
			IsPrimary:   a.IsPrimary,
			NIP05Active: a.NIP05Active,
			Active:      a.Active,
			Verified:    a.Verified,
			DisplayName: a.DisplayName,
			CreatedAt:   a.CreatedAt,
		})
	}

	response := AddressResponse{
		Username:    address.Username,
		Domain:      address.Domain,
		Pubkey:      address.Pubkey,
		Active:      address.Active,
		Verified:    address.Verified,
		IsPrimary:   address.IsPrimary,
		NIP05Active: address.NIP05Active,
		DisplayName: address.DisplayName,
		Addresses:   entries,
		Relays:      relays,
	}

	if lightning != nil {
		response.Lightning = &LightningConfigResponse{
			Mode:           lightning.Mode,
			ProxyAddress:   lightning.ProxyAddress,
			NWCConfigured:  lightning.NWCSecretEncrypted != "",
			NWCErrorCount:  lightning.NWCErrorCount,
			MinSendableSat: lightning.MinSendableMsats / 1000,
			MaxSendableSat: lightning.MaxSendableMsats / 1000,
			CommentAllowed: lightning.CommentAllowed,
			AllowsNostr:    lightning.AllowsNostr,
			Enabled:        lightning.Enabled,
		}
	}

	c.JSON(http.StatusOK, response)
}

// updateLightningConfig updates the user's Lightning Address configuration.
// Operates on the primary address.
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

	// Validate NWC configuration if mode is nwc
	var nwcConfig *nwc.ConnectionConfig
	if req.Mode == "nwc" {
		if req.NWCConnectionURI == nil || *req.NWCConnectionURI == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nwc_connection_uri required when mode is 'nwc'"})
			return
		}
		if h.nwcEncryptor == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "NWC mode is not available on this server"})
			return
		}
		// Parse the NWC connection URI
		var err error
		nwcConfig, err = nwc.ParseConnectionURI(*req.NWCConnectionURI)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid NWC connection URI: " + err.Error()})
			return
		}
	}

	// Get user's primary address (lightning config lives on the primary).
	address, err := h.store.GetPrimaryAddressByPubkey(ctx, pubkey)
	if err != nil {
		slog.Error("failed to get primary address", "pubkey", pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	if address == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No address registered for this pubkey"})
		return
	}

	// Build the update
	update := storage.LightningConfigUpdate{
		Mode: req.Mode,
	}

	if req.ProxyAddress != nil {
		update.ProxyAddress = *req.ProxyAddress
	}

	if nwcConfig != nil {
		update.NWCRelayURL = nwcConfig.RelayURL
		update.NWCWalletPubkey = nwcConfig.WalletPubkey

		// Encrypt the secret before storing
		encryptedSecret, err := h.nwcEncryptor.Encrypt(nwcConfig.Secret)
		if err != nil {
			slog.Error("failed to encrypt NWC secret", "address_id", address.ID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to secure NWC configuration"})
			return
		}
		update.NWCSecretEncrypted = encryptedSecret
	}

	err = h.store.UpsertLightningConfigFull(ctx, address.ID, update)
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
		NWCConfigured:  lightning.NWCSecretEncrypted != "",
		NWCErrorCount:  lightning.NWCErrorCount,
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

// transferAddress transfers the caller's PRIMARY address to another pubkey.
// Multi-address is now allowed: the transfer does NOT fail if the target already
// has addresses — the transferred address becomes primary only if the target has
// none, otherwise it becomes an alias.
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

	// Get user's primary address (self-serve transfer operates on the primary).
	address, err := h.store.GetPrimaryAddressByPubkey(ctx, pubkey)
	if err != nil {
		slog.Error("failed to get primary address", "pubkey", pubkey, "error", err)
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
				"error":         "Transfer cooldown active",
				"cooldown_ends": cooldownEnd.Format("2006-01-02T15:04:05Z"),
			})
			return
		}
	}

	// Perform transfer. TransferAddress now handles is_primary/nip05_active
	// reassignment and ownership intervals internally.
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
