package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
)

// --- Tenant management (NIP-98 authenticated, owner-gated) ---

// handleAddTenantMember adds a pubkey to a tenant.
// POST /api/v1/tenants/:id/members
// Body: {"pubkey":"<64hex>"}
// Auth: NIP-98 from the tenant owner.
func (h *Handler) handleAddTenantMember(c *gin.Context) {
	tenantID := c.Param("id")
	callerPubkey := c.GetString(auth.PubkeyContextKey)
	ctx := c.Request.Context()

	var req struct {
		Pubkey string `json:"pubkey" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if !validPubkey(req.Pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}

	tenant, err := h.store.GetTenant(ctx, tenantID)
	if err != nil {
		slog.Error("get tenant failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to look up tenant"})
		return
	}
	if tenant == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Tenant not found"})
		return
	}
	if tenant.OwnerPubkey != callerPubkey {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the tenant owner can manage members"})
		return
	}

	if err := h.store.AddTenantMember(ctx, tenantID, req.Pubkey); err != nil {
		slog.Error("add tenant member failed", "tenant", tenantID, "pubkey", req.Pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add member"})
		return
	}

	slog.Info("tenant member added",
		"tenant", tenantID,
		"pubkey", safePrefix(req.Pubkey),
		"by", safePrefix(callerPubkey),
	)
	c.JSON(http.StatusOK, gin.H{"success": true, "tenant_id": tenantID, "pubkey": req.Pubkey})
}

// handleRemoveTenantMember removes a pubkey from a tenant.
// DELETE /api/v1/tenants/:id/members/:pubkey
// Auth: NIP-98 from the tenant owner.
func (h *Handler) handleRemoveTenantMember(c *gin.Context) {
	tenantID := c.Param("id")
	pubkey := c.Param("pubkey")
	callerPubkey := c.GetString(auth.PubkeyContextKey)
	ctx := c.Request.Context()

	if !validPubkey(pubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pubkey format"})
		return
	}

	tenant, err := h.store.GetTenant(ctx, tenantID)
	if err != nil {
		slog.Error("get tenant failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to look up tenant"})
		return
	}
	if tenant == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Tenant not found"})
		return
	}
	if tenant.OwnerPubkey != callerPubkey {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the tenant owner can manage members"})
		return
	}

	if err := h.store.RemoveTenantMember(ctx, tenantID, pubkey); err != nil {
		slog.Error("remove tenant member failed", "tenant", tenantID, "pubkey", pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}

	slog.Info("tenant member removed",
		"tenant", tenantID,
		"pubkey", safePrefix(pubkey),
		"by", safePrefix(callerPubkey),
	)
	c.JSON(http.StatusOK, gin.H{"success": true, "tenant_id": tenantID, "removed": pubkey})
}

// handleListTenantMembers lists all members of a tenant.
// GET /api/v1/tenants/:id/members
// Auth: NIP-98 from the tenant owner.
func (h *Handler) handleListTenantMembers(c *gin.Context) {
	tenantID := c.Param("id")
	callerPubkey := c.GetString(auth.PubkeyContextKey)
	ctx := c.Request.Context()

	tenant, err := h.store.GetTenant(ctx, tenantID)
	if err != nil {
		slog.Error("get tenant failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to look up tenant"})
		return
	}
	if tenant == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Tenant not found"})
		return
	}
	if tenant.OwnerPubkey != callerPubkey {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the tenant owner can list members"})
		return
	}

	members, err := h.store.ListTenantMembers(ctx, tenantID)
	if err != nil {
		slog.Error("list tenant members failed", "tenant", tenantID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list members"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"tenant_id": tenantID, "members": members, "count": len(members)})
}

// --- Admin tenant management ---

// adminCreateTenant creates a new tenant.
// POST /admin/v1/tenants
// Body: {"id":"arbiter-fleet","owner_pubkey":"<64hex>"}
func (h *Handler) adminCreateTenant(c *gin.Context) {
	var req struct {
		ID          string `json:"id" binding:"required"`
		OwnerPubkey string `json:"owner_pubkey" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if !validPubkey(req.OwnerPubkey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid owner pubkey format"})
		return
	}

	tenant, err := h.store.CreateTenant(c.Request.Context(), req.ID, req.OwnerPubkey)
	if err != nil {
		slog.Error("create tenant failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create tenant"})
		return
	}

	slog.Info("tenant created", "id", tenant.ID, "owner", safePrefix(req.OwnerPubkey))
	c.JSON(http.StatusCreated, gin.H{"success": true, "tenant": tenant})
}

// adminSetTenantQuota sets the quota limit for a tenant.
// POST /admin/v1/tenants/quota
// Body: {"tenant_id":"arbiter-fleet","quota_type":"storage_bytes","limit":10737418240}
func (h *Handler) adminSetTenantQuota(c *gin.Context) {
	var req struct {
		TenantID  string `json:"tenant_id" binding:"required"`
		QuotaType string `json:"quota_type" binding:"required"`
		Limit     int64  `json:"limit" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	tenant, err := h.store.GetTenant(c.Request.Context(), req.TenantID)
	if err != nil {
		slog.Error("get tenant for quota failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to look up tenant"})
		return
	}
	if tenant == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Tenant not found"})
		return
	}

	if err := h.store.SetTenantQuota(c.Request.Context(), req.TenantID, req.QuotaType, req.Limit); err != nil {
		slog.Error("set tenant quota failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to set tenant quota"})
		return
	}

	slog.Info("tenant quota set",
		"tenant", req.TenantID,
		"quota_type", req.QuotaType,
		"limit", req.Limit,
	)
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"tenant_id":  req.TenantID,
		"quota_type": req.QuotaType,
		"limit":      req.Limit,
	})
}
