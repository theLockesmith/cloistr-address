package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// PurchaseQuoteRequest represents a quote request
type PurchaseQuoteRequest struct {
	Username string `json:"username" binding:"required"`
}

// PurchaseQuoteResponse represents a purchase quote
type PurchaseQuoteResponse struct {
	Username   string `json:"username"`
	Available  bool   `json:"available"`
	PriceSats  int64  `json:"price_sats"`
	Tier       string `json:"tier"`
	ValidUntil string `json:"valid_until"`
}

// PurchaseInvoiceRequest represents an invoice creation request
type PurchaseInvoiceRequest struct {
	Username string `json:"username" binding:"required"`
}

// PurchaseInvoiceResponse represents a created invoice
type PurchaseInvoiceResponse struct {
	PaymentID      int64  `json:"payment_id"`
	AmountSats     int64  `json:"amount_sats"`
	PaymentRequest string `json:"payment_request"` // BOLT11 invoice
	ExpiresAt      string `json:"expires_at"`
}

// PurchaseStatusResponse represents payment status
type PurchaseStatusResponse struct {
	PaymentID  int64  `json:"payment_id"`
	Status     string `json:"status"` // pending, paid, expired
	AmountSats int64  `json:"amount_sats"`
	Username   string `json:"username,omitempty"` // Registered username if paid
}

// getPurchaseQuote returns a quote for purchasing a username
// POST /api/v1/purchase/quote
// TODO: Requires NIP-98 authentication middleware
func (h *Handler) getPurchaseQuote(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": "Purchase flow not yet implemented",
	})
}

// createPurchaseInvoice creates a Lightning invoice for purchasing a username
// POST /api/v1/purchase/invoice
// TODO: Requires NIP-98 authentication middleware and LND integration
func (h *Handler) createPurchaseInvoice(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": "Purchase flow not yet implemented",
	})
}

// getPurchaseStatus checks the status of a purchase
// GET /api/v1/purchase/status/:id
func (h *Handler) getPurchaseStatus(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": "Purchase flow not yet implemented",
	})
}
