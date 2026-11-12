package api

import (
	"log/slog"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/btcpay"
)

// Statuses the purchase page understands. It stops polling on completed,
// failed and expired, and shows anything else as-is.
const (
	PurchaseStatusPending    = "pending"
	PurchaseStatusProcessing = "processing"
	PurchaseStatusCompleted  = "completed"
	PurchaseStatusExpired    = "expired"
	PurchaseStatusFailed     = "failed"
)

// BTCPay invoice ids are short alphanumerics. Anything else never reaches the
// BTCPay URL, which this id is interpolated into.
var invoiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// PurchaseStatusResponse is what the purchase page polls for.
type PurchaseStatusResponse struct {
	Status    string `json:"status"`
	Username  string `json:"username,omitempty"`
	ProductID string `json:"product_id,omitempty"`
	// Reason explains a failed settled purchase: "name_taken" means another
	// payment won the race and the webhook credited this one instead.
	Reason string `json:"reason,omitempty"`
}

// getPurchaseStatus reports where an address or product purchase stands.
// For a product (storage top-up), "completed" means the webhook has recorded
// the quota grant for this invoice.
// GET /api/v1/purchase/status/:invoiceId
//
// The purchase page polled this route for as long as it existed, but it was
// never registered, so a paid name sat on the QR code forever.
//
// Only the pubkey the invoice was minted for can see it; any other caller,
// and any invoice that is not an address purchase, gets 404. "completed"
// means the address is actually registered to the caller, not merely that
// BTCPay settled: the webhook registers it, and until it has, the purchase
// is still "processing". If the webhook gave the name to an earlier payment,
// this one was credited instead, and the purchase is "failed" with a reason.
func (h *Handler) getPurchaseStatus(c *gin.Context) {
	ctx := c.Request.Context()
	pubkey := auth.GetPubkey(c)
	invoiceID := c.Param("invoiceId")

	if !invoiceIDPattern.MatchString(invoiceID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invoice not found"})
		return
	}

	invoice, err := h.btcpay.GetInvoice(ctx, invoiceID)
	if err != nil {
		slog.Warn("purchase status: invoice lookup failed", "invoice_id", invoiceID, "error", err)
		c.JSON(http.StatusNotFound, gin.H{"error": "Invoice not found"})
		return
	}

	owner, _ := invoice.Metadata[MetaPubkey].(string)
	username, _ := invoice.Metadata[MetaUsername].(string)
	kind := settlementKind(invoice.Metadata)
	if owner == "" || owner != pubkey || (kind != KindAddress && kind != KindProduct) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invoice not found"})
		return
	}
	if kind == KindProduct {
		h.productPurchaseStatus(c, invoiceID, invoice)
		return
	}

	resp := PurchaseStatusResponse{Username: username}
	switch invoice.Status {
	case btcpay.StatusNew:
		resp.Status = PurchaseStatusPending
	case btcpay.StatusProcessing:
		resp.Status = PurchaseStatusProcessing
	case btcpay.StatusExpired:
		resp.Status = PurchaseStatusExpired
	case btcpay.StatusInvalid:
		resp.Status = PurchaseStatusFailed
	case btcpay.StatusSettled:
		addr, err := h.store.GetAddressByUsername(ctx, username, h.cfg.Domain)
		if err != nil {
			slog.Error("purchase status: address lookup failed", "invoice_id", invoiceID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
			return
		}
		switch {
		case addr == nil:
			resp.Status = PurchaseStatusProcessing
		case addr.Pubkey == pubkey:
			resp.Status = PurchaseStatusCompleted
		default:
			resp.Status = PurchaseStatusFailed
			resp.Reason = "name_taken"
		}
	default:
		resp.Status = PurchaseStatusPending
	}

	c.JSON(http.StatusOK, resp)
}

// productPurchaseStatus is getPurchaseStatus for a product invoice, which the
// caller has already been checked to own.
func (h *Handler) productPurchaseStatus(c *gin.Context, invoiceID string, invoice *btcpay.Invoice) {
	productID, _ := invoice.Metadata[MetaProductID].(string)
	resp := PurchaseStatusResponse{ProductID: productID}
	switch invoice.Status {
	case btcpay.StatusNew:
		resp.Status = PurchaseStatusPending
	case btcpay.StatusProcessing:
		resp.Status = PurchaseStatusProcessing
	case btcpay.StatusExpired:
		resp.Status = PurchaseStatusExpired
	case btcpay.StatusInvalid:
		resp.Status = PurchaseStatusFailed
	case btcpay.StatusSettled:
		granted, err := h.store.QuotaGrantExists(c.Request.Context(), invoiceID)
		if err != nil {
			slog.Error("purchase status: grant lookup failed", "invoice_id", invoiceID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
			return
		}
		if granted {
			resp.Status = PurchaseStatusCompleted
		} else {
			resp.Status = PurchaseStatusProcessing
		}
	default:
		resp.Status = PurchaseStatusPending
	}
	c.JSON(http.StatusOK, resp)
}
