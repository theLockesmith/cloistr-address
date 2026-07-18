package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nbd-wtf/go-nostr"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// Context keys set by adminAuthMiddleware.
const (
	adminPubkeyKey = "admin_pubkey"
	adminSigKey    = "admin_sig"
)

// registerAdminRoutes wires the internal admin interface under /admin/v1.
// Every route is authenticated by a NIP-98 signed request AND authorized against
// users.is_platform_admin. Mutations additionally bind the request body to the
// signature via the NIP-98 "payload" tag. All mutations write a signed,
// hash-chained audit_log row (see internal/storage/admin.go).
func (h *Handler) registerAdminRoutes(r *gin.Engine) {
	admin := r.Group("/admin/v1")
	admin.Use(h.adminAuthMiddleware())
	{
		// Session / gating
		admin.GET("/me", h.adminWhoami)

		// Addresses
		admin.GET("/addresses", h.adminListAddresses)
		admin.POST("/addresses/grant", h.adminGrantAddress)
		admin.POST("/addresses/revoke", h.adminRevokeAddress)
		admin.POST("/addresses/transfer", h.adminTransferAddress)
		admin.POST("/addresses/primary", h.adminSetAddressPrimary)
		admin.POST("/addresses/nip05", h.adminSetAddressNIP05)

		// User lookup (name → pubkey + all addresses)
		admin.GET("/users/lookup", h.adminLookupUser)

		// Reserved usernames
		admin.GET("/reserved", h.adminListReserved)
		admin.POST("/reserved", h.adminAddReserved)
		admin.DELETE("/reserved/:username", h.adminRemoveReserved)

		// Quotas
		admin.GET("/quotas", h.adminGetQuotas)
		admin.POST("/quotas", h.adminSetQuota)
		admin.POST("/quotas/reset", h.adminResetQuota)

		// Credits
		admin.GET("/credits", h.adminGetCredits)
		admin.POST("/credits/grant", h.adminGrantCredits)

		// Tiers
		admin.GET("/tiers", h.adminListTiers)
		admin.PUT("/tiers", h.adminUpdateTier)

		// Audit
		admin.GET("/audit", h.adminListAudit)
	}
}

// adminAuthMiddleware authenticates via NIP-98, binds the body to the signature
// for mutations, and authorizes the pubkey as a platform admin.
func (h *Handler) adminAuthMiddleware() gin.HandlerFunc {
	validator := auth.NewNIP98Validator(auth.DefaultNIP98Config())
	return func(c *gin.Context) {
		// 1. Core NIP-98 validation (signature, drift, URL, method) -> pubkey.
		pubkey, err := validator.ValidateRequest(c.Request)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":   "Authentication required",
				"details": err.Error(),
			})
			return
		}

		// 2. Re-parse the signed event to recover its signature and payload tag.
		event, err := parseNIP98Event(c.Request)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid auth event"})
			return
		}

		// 3. For body-bearing methods, bind the body hash to the signature.
		if bodyBearing(c.Request.Method) {
			body, err := io.ReadAll(c.Request.Body)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Cannot read body"})
				return
			}
			// Restore the body for the handler's ShouldBindJSON.
			c.Request.Body = io.NopCloser(bytes.NewReader(body))

			if len(body) > 0 {
				want := getTag(event.Tags, "payload")
				if want == "" {
					c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
						"error": "Signed request missing payload tag for body",
					})
					return
				}
				sum := sha256.Sum256(body)
				if !strings.EqualFold(want, hex.EncodeToString(sum[:])) {
					c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
						"error": "Body does not match signed payload hash",
					})
					return
				}
			}
		}

		// 4. Authorize: platform admin only for the /admin surface.
		isAdmin, err := h.store.IsPlatformAdmin(c.Request.Context(), pubkey)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "authz check failed"})
			return
		}
		if !isAdmin {
			slog.Warn("admin API access denied", "pubkey", safePrefix(pubkey), "path", c.Request.URL.Path)
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Not a platform admin"})
			return
		}

		c.Set(adminPubkeyKey, pubkey)
		c.Set(adminSigKey, event.Sig)
		c.Next()
	}
}

// parseNIP98Event decodes the Authorization: Nostr <base64> header into an event.
func parseNIP98Event(r *http.Request) (*nostr.Event, error) {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Nostr ") {
		return nil, errors.New("invalid auth format")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authHeader, "Nostr "))
	if err != nil {
		return nil, err
	}
	var ev nostr.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

func getTag(tags nostr.Tags, name string) string {
	for _, t := range tags {
		if len(t) >= 2 && t[0] == name {
			return t[1]
		}
	}
	return ""
}

func bodyBearing(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

func safePrefix(pubkey string) string {
	if len(pubkey) >= 8 {
		return pubkey[:8] + "..."
	}
	return pubkey
}

func adminActor(c *gin.Context) (pubkey, sig string) {
	if v, ok := c.Get(adminPubkeyKey); ok {
		pubkey, _ = v.(string)
	}
	if v, ok := c.Get(adminSigKey); ok {
		sig, _ = v.(string)
	}
	return
}

// abortStore maps store errors to HTTP responses.
func abortStore(c *gin.Context, err error, msg string) {
	if errors.Is(err, storage.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	slog.Error(msg, "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": msg})
}

func validPubkey(pk string) bool {
	if len(pk) != 64 {
		return false
	}
	_, err := hex.DecodeString(pk)
	return err == nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) adminWhoami(c *gin.Context) {
	pubkey, _ := adminActor(c)
	c.JSON(http.StatusOK, gin.H{"pubkey": pubkey, "is_platform_admin": true})
}

// --- Addresses ---

type adminGrantAddressRequest struct {
	Pubkey      string  `json:"pubkey" binding:"required"`
	Username    string  `json:"username" binding:"required"`
	Domain      string  `json:"domain,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
}

func (h *Handler) adminGrantAddress(c *gin.Context) {
	var req adminGrantAddressRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if !validPubkey(req.Pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}
	domain := req.Domain
	if domain == "" {
		domain = h.cfg.Domain
	}
	actor, sig := adminActor(c)
	addr, err := h.store.AdminGrantAddress(c.Request.Context(), actor, sig, strings.ToLower(req.Username), domain, req.Pubkey, req.DisplayName)
	if err != nil {
		abortStore(c, err, "Failed to grant address")
		return
	}
	slog.Info("admin granted address",
		"username", addr.Username, "domain", addr.Domain,
		"is_primary", addr.IsPrimary, "nip05_active", addr.NIP05Active,
		"pubkey", safePrefix(req.Pubkey), "actor", safePrefix(actor),
	)
	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"address_id":   addr.ID,
		"username":     addr.Username,
		"domain":       addr.Domain,
		"is_primary":   addr.IsPrimary,
		"nip05_active": addr.NIP05Active,
	})
}

type adminRevokeAddressRequest struct {
	Username string `json:"username" binding:"required"`
	Domain   string `json:"domain,omitempty"`
	Reason   string `json:"reason" binding:"required"`
}

func (h *Handler) adminRevokeAddress(c *gin.Context) {
	var req adminRevokeAddressRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	domain := req.Domain
	if domain == "" {
		domain = h.cfg.Domain
	}
	actor, sig := adminActor(c)
	if err := h.store.AdminRevokeAddress(c.Request.Context(), actor, sig, strings.ToLower(req.Username), domain, req.Reason); err != nil {
		abortStore(c, err, "Failed to revoke address")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

type adminTransferAddressRequest struct {
	Username string `json:"username" binding:"required"`
	Domain   string `json:"domain,omitempty"`
	ToPubkey string `json:"to_pubkey" binding:"required"`
}

func (h *Handler) adminTransferAddress(c *gin.Context) {
	var req adminTransferAddressRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if !validPubkey(req.ToPubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid to_pubkey format"})
		return
	}
	domain := req.Domain
	if domain == "" {
		domain = h.cfg.Domain
	}
	actor, sig := adminActor(c)
	if err := h.store.AdminTransferAddress(c.Request.Context(), actor, sig, strings.ToLower(req.Username), domain, req.ToPubkey); err != nil {
		abortStore(c, err, "Failed to transfer address")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) adminListAddresses(c *gin.Context) {
	pubkey := c.Query("pubkey")
	if !validPubkey(pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pubkey query param required (64 hex)"})
		return
	}
	addrs, err := h.store.AdminListAddressesByPubkey(c.Request.Context(), pubkey)
	if err != nil {
		abortStore(c, err, "Failed to list addresses")
		return
	}
	c.JSON(http.StatusOK, gin.H{"addresses": addrs})
}

// adminSetAddressPrimary flips is_primary for the given (username, domain, pubkey).
// POST /admin/v1/addresses/primary
// Body: {"username":"alice","domain":"cloistr.xyz","pubkey":"<64hex>"}
func (h *Handler) adminSetAddressPrimary(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required"`
		Domain   string `json:"domain,omitempty"`
		Pubkey   string `json:"pubkey" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if !validPubkey(req.Pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}
	domain := req.Domain
	if domain == "" {
		domain = h.cfg.Domain
	}
	actor, sig := adminActor(c)
	if err := h.store.SetAddressPrimary(c.Request.Context(), actor, sig, strings.ToLower(req.Username), domain, req.Pubkey); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		slog.Error("set_primary failed", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminSetAddressNIP05 flips nip05_active for the given (username, domain, pubkey).
// POST /admin/v1/addresses/nip05
// Body: {"username":"alice","domain":"cloistr.xyz","pubkey":"<64hex>"}
func (h *Handler) adminSetAddressNIP05(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required"`
		Domain   string `json:"domain,omitempty"`
		Pubkey   string `json:"pubkey" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if !validPubkey(req.Pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}
	domain := req.Domain
	if domain == "" {
		domain = h.cfg.Domain
	}
	actor, sig := adminActor(c)
	if err := h.store.SetAddressNIP05(c.Request.Context(), actor, sig, strings.ToLower(req.Username), domain, req.Pubkey); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		slog.Error("set_nip05 failed", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminLookupUser resolves a canonical username to a pubkey and all its active addresses.
// GET /admin/v1/users/lookup?name=alice&domain=cloistr.xyz
func (h *Handler) adminLookupUser(c *gin.Context) {
	name := strings.ToLower(c.Query("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name query param required"})
		return
	}
	domain := c.Query("domain")
	if domain == "" {
		domain = h.cfg.Domain
	}

	result, err := h.store.AdminLookupUser(c.Request.Context(), name, domain)
	if err != nil {
		abortStore(c, err, "Failed to lookup user")
		return
	}
	if result == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, result)
}

// --- Reserved ---

type adminAddReservedRequest struct {
	Username  string  `json:"username" binding:"required"`
	ForPubkey *string `json:"for_pubkey,omitempty"` // nil = block entirely
	Reason    string  `json:"reason,omitempty"`
}

func (h *Handler) adminAddReserved(c *gin.Context) {
	var req adminAddReservedRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if req.ForPubkey != nil && !validPubkey(*req.ForPubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid for_pubkey format"})
		return
	}
	actor, sig := adminActor(c)
	if err := h.store.AddReserved(c.Request.Context(), actor, sig, strings.ToLower(req.Username), req.ForPubkey, req.Reason); err != nil {
		abortStore(c, err, "Failed to add reservation")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) adminRemoveReserved(c *gin.Context) {
	username := strings.ToLower(c.Param("username"))
	actor, sig := adminActor(c)
	if err := h.store.RemoveReserved(c.Request.Context(), actor, sig, username); err != nil {
		abortStore(c, err, "Failed to remove reservation")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) adminListReserved(c *gin.Context) {
	items, err := h.store.ListReserved(c.Request.Context())
	if err != nil {
		abortStore(c, err, "Failed to list reservations")
		return
	}
	c.JSON(http.StatusOK, gin.H{"reserved": items})
}

// --- Quotas ---

type adminSetQuotaRequest struct {
	Pubkey      string `json:"pubkey" binding:"required"`
	QuotaTypeID string `json:"quota_type_id" binding:"required"`
	Limit       int64  `json:"limit"` // 0 = unlimited
}

func (h *Handler) adminSetQuota(c *gin.Context) {
	var req adminSetQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if !validPubkey(req.Pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}
	if req.Limit < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be >= 0 (0 = unlimited)"})
		return
	}
	actor, sig := adminActor(c)
	if err := h.store.SetQuota(c.Request.Context(), actor, sig, req.Pubkey, req.QuotaTypeID, req.Limit); err != nil {
		abortStore(c, err, "Failed to set quota")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

type adminResetQuotaRequest struct {
	Pubkey      string `json:"pubkey" binding:"required"`
	QuotaTypeID string `json:"quota_type_id" binding:"required"`
}

func (h *Handler) adminResetQuota(c *gin.Context) {
	var req adminResetQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	actor, sig := adminActor(c)
	if err := h.store.ResetQuota(c.Request.Context(), actor, sig, req.Pubkey, req.QuotaTypeID); err != nil {
		abortStore(c, err, "Failed to reset quota")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) adminGetQuotas(c *gin.Context) {
	pubkey := c.Query("pubkey")
	if !validPubkey(pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pubkey query param required (64 hex)"})
		return
	}
	quotas, err := h.store.GetQuotas(c.Request.Context(), pubkey)
	if err != nil {
		abortStore(c, err, "Failed to get quotas")
		return
	}
	c.JSON(http.StatusOK, gin.H{"quotas": quotas})
}

// --- Credits ---

func (h *Handler) adminGetCredits(c *gin.Context) {
	pubkey := c.Query("pubkey")
	if !validPubkey(pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pubkey query param required (64 hex)"})
		return
	}
	balance, err := h.store.GetCredits(c.Request.Context(), pubkey)
	if err != nil {
		abortStore(c, err, "Failed to get credits")
		return
	}
	c.JSON(http.StatusOK, gin.H{"pubkey": pubkey, "balance_sats": balance})
}

func (h *Handler) adminGrantCredits(c *gin.Context) {
	var req GrantCreditsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if !validPubkey(req.Pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}
	if req.Reason == "" {
		req.Reason = "admin_grant"
	}
	actor, sig := adminActor(c)
	ctx := c.Request.Context()
	if err := h.store.AddCredits(ctx, req.Pubkey, req.AmountSats, req.Reason, req.ReferenceID); err != nil {
		abortStore(c, err, "Failed to grant credits")
		return
	}
	// credit_history already records the change; add a signed admin audit row too.
	if err := h.store.LogAdminAudit(ctx, storage.AuditEntry{
		TableName:     "pubkey_credits",
		RecordID:      req.Pubkey,
		Action:        "credits.grant",
		ActorPubkey:   actor,
		SubjectPubkey: req.Pubkey,
		NewValues:     map[string]any{"amount_sats": req.AmountSats, "reason": req.Reason, "reference_id": req.ReferenceID},
		Metadata:      map[string]any{"source": "admin"},
		Signature:     sig,
	}); err != nil {
		slog.Warn("credits granted but audit write failed", "pubkey", safePrefix(req.Pubkey), "error", err)
	}
	balance, _ := h.store.GetCredits(ctx, req.Pubkey)
	c.JSON(http.StatusOK, GrantCreditsResponse{
		Success: true, Pubkey: req.Pubkey, AmountSats: req.AmountSats, NewBalance: balance, ReferenceID: req.ReferenceID,
	})
}

// --- Tiers ---

func (h *Handler) adminListTiers(c *gin.Context) {
	tiers, err := h.store.ListTiers(c.Request.Context())
	if err != nil {
		abortStore(c, err, "Failed to list tiers")
		return
	}
	c.JSON(http.StatusOK, gin.H{"tiers": tiers})
}

type adminUpdateTierRequest struct {
	TierName  string `json:"tier_name" binding:"required"`
	PriceSats int64  `json:"price_sats"`
	Enabled   bool   `json:"enabled"`
}

func (h *Handler) adminUpdateTier(c *gin.Context) {
	var req adminUpdateTierRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if req.PriceSats < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "price_sats must be >= 0"})
		return
	}
	actor, sig := adminActor(c)
	if err := h.store.UpdateTier(c.Request.Context(), actor, sig, req.TierName, req.PriceSats, req.Enabled); err != nil {
		abortStore(c, err, "Failed to update tier")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// --- Audit ---

// adminListAudit returns audit log entries with optional filters.
// GET /admin/v1/audit
// Query params:
//
//	action   — exact action match (e.g. "address.grant")
//	actor    — actor pubkey (hex)
//	subject  — subject pubkey (hex) — direct match on audit_log.subject_pubkey
//	name     — canonical username, resolved temporally via address_ownership
//	domain   — domain for name resolution (defaults to server domain if name set)
//	start    — RFC3339 lower bound on created_at (inclusive)
//	end      — RFC3339 upper bound on created_at (inclusive)
//	limit    — max rows (default 100, max 500)
//	offset   — pagination offset
func (h *Handler) adminListAudit(c *gin.Context) {
	f := storage.AuditListFilter{
		Action:  c.Query("action"),
		Actor:   c.Query("actor"),
		Subject: c.Query("subject"),
		Name:    strings.ToLower(c.Query("name")),
		Domain:  c.Query("domain"),
	}

	// Default domain for name resolution.
	if f.Name != "" && f.Domain == "" {
		f.Domain = h.cfg.Domain
	}

	if s := c.Query("start"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.Start = &t
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start: must be RFC3339"})
			return
		}
	}
	if e := c.Query("end"); e != "" {
		if t, err := time.Parse(time.RFC3339, e); err == nil {
			f.End = &t
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end: must be RFC3339"})
			return
		}
	}
	f.Limit, _ = strconv.Atoi(c.Query("limit"))
	f.Offset, _ = strconv.Atoi(c.Query("offset"))

	rows, err := h.store.ListAudit(c.Request.Context(), f)
	if err != nil {
		abortStore(c, err, "Failed to list audit log")
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": rows})
}
