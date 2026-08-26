package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// ProductInvoiceRequest asks for an invoice against a catalog product.
type ProductInvoiceRequest struct {
	ProductID string `json:"product_id" binding:"required"`
	// Pubkey is who receives the grant. Ignored on the NIP-98 route (the caller
	// IS the buyer); required on the internal route, where the calling service
	// is buying on a user's behalf.
	Pubkey string `json:"pubkey,omitempty"`
}

// ProductInvoiceResponse is the invoice to pay.
type ProductInvoiceResponse struct {
	InvoiceID   string `json:"invoice_id"`
	ProductID   string `json:"product_id"`
	DisplayName string `json:"display_name"`
	PriceSats   int64  `json:"price_sats"`
	CheckoutURL string `json:"checkout_url,omitempty"`
	PaymentURL  string `json:"payment_url,omitempty"`
	// Status is "invoiced" normally, or "granted" when the product costs nothing
	// and was applied immediately.
	Status string `json:"status"`
}

// createProductInvoice mints an invoice for a catalog product.
//
// # WHY THIS EXISTS
//
// createPurchaseInvoice can only sell a username: it takes a username, prices it
// by length, and its settlement path registers an address. Everything else in
// the catalog — storage_topup_10/50/100, priced since migration 006 — had no way
// to be bought at all.
//
// Services do NOT get their own BTCPay credentials. stash asks for an invoice
// over /internal/v1 and gets back something to show the user; the grant lands in
// the same shared database the quota check already reads.
func (h *Handler) createProductInvoice(c *gin.Context, buyerPubkey string) {
	ctx := c.Request.Context()

	var req ProductInvoiceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request", "details": err.Error()})
		return
	}
	if buyerPubkey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing pubkey"})
		return
	}

	product, err := h.store.GetProduct(ctx, req.ProductID)
	if err != nil {
		if errors.Is(err, storage.ErrProductNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Unknown product"})
			return
		}
		slog.Error("failed to read product", "product_id", req.ProductID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}
	if !product.Enabled {
		// A disabled product is withdrawn from sale, not free.
		c.JSON(http.StatusConflict, gin.H{"error": "Product is not available"})
		return
	}

	// A zero-price product needs no payment rail at all. Granting it directly
	// keeps free tiers working even when BTCPay is unreachable.
	if product.PriceSats == 0 {
		outcome, err := h.settleProduct(ctx, "free:"+product.ID+":"+buyerPubkey, buyerPubkey, product.ID, timeNow())
		if err != nil {
			slog.Error("failed to grant free product", "product_id", product.ID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
			return
		}
		c.JSON(http.StatusOK, ProductInvoiceResponse{
			ProductID:   product.ID,
			DisplayName: product.DisplayName,
			PriceSats:   0,
			Status:      outcome.Status,
		})
		return
	}

	if !h.btcpay.IsConfigured() {
		// Say which knob is missing. This answered a bare 503 for weeks while
		// BTCPAY_URL sat empty in the ConfigMap.
		slog.Error("product invoice requested but BTCPay is not configured",
			"product_id", product.ID,
			"hint", "BTCPAY_URL / BTCPAY_API_KEY / BTCPAY_STORE_ID must all be set")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Payment system unavailable"})
		return
	}

	invoice, err := h.btcpay.CreateInvoice(product.PriceSats, map[string]interface{}{
		MetaKind:      KindProduct,
		MetaProductID: product.ID,
		MetaPubkey:    buyerPubkey,
	})
	if err != nil {
		slog.Error("failed to create product invoice",
			"product_id", product.ID, "pubkey", buyerPubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create invoice"})
		return
	}

	slog.Info("product invoice created",
		"product_id", product.ID, "pubkey", buyerPubkey,
		"price_sats", product.PriceSats, "invoice_id", invoice.ID)

	c.JSON(http.StatusOK, ProductInvoiceResponse{
		InvoiceID:   invoice.ID,
		ProductID:   product.ID,
		DisplayName: product.DisplayName,
		PriceSats:   product.PriceSats,
		CheckoutURL: invoice.CheckoutLink,
		Status:      "invoiced",
	})
}

// purchaseProduct is the user-facing route: the caller is the buyer.
func (h *Handler) purchaseProduct(c *gin.Context) {
	h.createProductInvoice(c, auth.GetPubkey(c))
}

// internalPurchaseProduct is the service-to-service route: stash asks for an
// invoice on a user's behalf, so the pubkey comes from the body.
func (h *Handler) internalPurchaseProduct(c *gin.Context) {
	var probe ProductInvoiceRequest
	if err := c.ShouldBindBodyWithJSON(&probe); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request", "details": err.Error()})
		return
	}
	h.createProductInvoice(c, probe.Pubkey)
}
