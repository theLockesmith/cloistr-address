package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// StorageTopup is one storage top-up on sale.
type StorageTopup struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	Description  string `json:"description"`
	PriceSats    int64  `json:"price_sats"`
	StorageBytes int64  `json:"storage_bytes"`
}

// StorageTopupsResponse lists the storage top-ups on sale.
type StorageTopupsResponse struct {
	Products []StorageTopup `json:"products"`
}

// listStorageTopups lists the storage top-ups on sale, from the catalog.
// GET /api/v1/products/storage
//
// Public, like listPricingTiers: these are list prices. A lookup failure is a
// 500, never a partial list, for the same reason.
func (h *Handler) listStorageTopups(c *gin.Context) {
	topups, err := h.store.ListStorageTopups(c.Request.Context())
	if err != nil {
		slog.Error("failed to list storage top-ups", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}
	out := make([]StorageTopup, 0, len(topups))
	for _, p := range topups {
		out = append(out, StorageTopup{
			ID: p.ID, DisplayName: p.DisplayName, Description: p.Description,
			PriceSats: p.PriceSats, StorageBytes: p.StorageBytes,
		})
	}
	c.JSON(http.StatusOK, StorageTopupsResponse{Products: out})
}
