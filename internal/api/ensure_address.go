package api

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

var hexPubkey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// EnsureAddressRequest asks for a pubkey's address, creating the auto-assigned
// one if it has none.
type EnsureAddressRequest struct {
	Pubkey string `json:"pubkey"`
}

// EnsureAddressResponse is the pubkey's primary address after the ensure.
type EnsureAddressResponse struct {
	Username string `json:"username"`
	Domain   string `json:"domain"`
	Pubkey   string `json:"pubkey"`
	Verified bool   `json:"verified"`
	// Created is true when this call created the address.
	Created bool `json:"created"`
}

// isSyncEnsurePath reports whether a request ensures the caller's address itself,
// synchronously, so the middleware must not also race it from a goroutine.
func isSyncEnsurePath(path string) bool {
	return strings.HasSuffix(path, "/addresses/me")
}

// ensurePrimaryAddress returns the pubkey's primary address, creating the
// auto-assigned one first if the pubkey has no active address. created reports
// whether this call made it.
//
// Two concurrent ensures for one pubkey both see zero addresses; the loser's
// insert fails on the one-primary-per-pubkey index. That error is not fatal: the
// winner's address is read back below.
func (h *Handler) ensurePrimaryAddress(ctx context.Context, pubkey string) (addr *storage.Address, created bool, err error) {
	addr, err = h.store.GetPrimaryAddressByPubkey(ctx, pubkey)
	if err != nil || addr != nil {
		return addr, false, err
	}
	made, ensureErr := h.store.EnsureAutoAddress(ctx, pubkey, h.cfg.Domain)
	if ensureErr != nil {
		slog.Warn("ensure auto address failed; reading back", "pubkey", pubkey, "error", ensureErr)
	}
	addr, err = h.store.GetPrimaryAddressByPubkey(ctx, pubkey)
	return addr, made != nil && addr != nil && made.ID == addr.ID, err
}

// internalEnsureAddress gives a pubkey its address without the user ever opening
// Me. cloistr-email calls it when someone signs in to Mail with no address:
// before this, an identity that signed up through Mail had no address at all
// until it happened to visit the Me dashboard.
// POST /internal/v1/addresses/ensure
func (h *Handler) internalEnsureAddress(c *gin.Context) {
	ctx := c.Request.Context()

	var req EnsureAddressRequest
	if err := c.ShouldBindJSON(&req); err != nil || !hexPubkey.MatchString(req.Pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pubkey must be 64 lowercase hex characters"})
		return
	}

	if err := h.store.EnsureUser(ctx, req.Pubkey); err != nil {
		slog.Error("ensure user failed", "pubkey", req.Pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}
	addr, created, err := h.ensurePrimaryAddress(ctx, req.Pubkey)
	if err != nil || addr == nil {
		slog.Error("ensure address failed", "pubkey", req.Pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	c.JSON(http.StatusOK, EnsureAddressResponse{
		Username: addr.Username,
		Domain:   addr.Domain,
		Pubkey:   addr.Pubkey,
		Verified: addr.Verified,
		Created:  created,
	})
}
