package api

import (
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/btcpay"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// handleBTCPayWebhook processes BTCPay webhook notifications
// This handles invoice settlement events for username registration
func (h *Handler) handleBTCPayWebhook(c *gin.Context) {
	ctx := c.Request.Context()

	// Read body for signature verification
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		slog.Error("failed to read webhook body", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read body"})
		return
	}

	// Verify webhook signature
	signature := c.GetHeader("BTCPay-Sig")
	if !h.btcpay.VerifyWebhookSignature(body, signature) {
		slog.Warn("invalid webhook signature", "signature", signature)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
		return
	}

	// Parse webhook event
	event, err := h.btcpay.ParseWebhookEvent(body)
	if err != nil {
		slog.Error("failed to parse webhook event", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid event format"})
		return
	}

	slog.Info("received BTCPay webhook",
		"type", event.Type,
		"invoice_id", event.InvoiceID,
		"store_id", event.StoreID,
	)

	// Only process settlement events
	if event.Type != btcpay.EventInvoiceSettled {
		// Acknowledge other events but don't process
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "type": event.Type})
		return
	}

	pubkey, ok := event.Metadata[MetaPubkey].(string)
	if !ok || pubkey == "" {
		slog.Error("webhook missing pubkey in metadata", "invoice_id", event.InvoiceID)
		c.JSON(http.StatusOK, gin.H{"status": "error", "message": "Missing pubkey"})
		return
	}

	// Dispatch on WHAT was bought. This used to read metadata["username"] and
	// nothing else, so a settled invoice for any other product logged
	// "Missing username" and dropped the payment — which is why the storage
	// top-ups have been priced in the catalog since migration 006 with no way to
	// sell them.
	switch settlementKind(event.Metadata) {
	case KindProduct:
		productID, _ := event.Metadata[MetaProductID].(string)
		outcome, err := h.settleProduct(ctx, event.InvoiceID, pubkey, productID, time.Now())
		if err != nil {
			// 500, deliberately: BTCPay retries on a non-2xx, and a retry is
			// exactly what we want when the grant failed. Answering 200 here
			// would take the user's money and lose the thing they bought.
			slog.Error("product settlement failed",
				"invoice_id", event.InvoiceID, "product_id", productID,
				"pubkey", pubkey, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Settlement failed"})
			return
		}
		resp := gin.H{"status": outcome.Status, "message": outcome.Message}
		for k, v := range outcome.Detail {
			resp[k] = v
		}
		c.JSON(http.StatusOK, resp)
		return

	case KindAddress:
		// falls through to the address flow below

	default:
		slog.Error("webhook metadata names nothing purchasable",
			"invoice_id", event.InvoiceID, "metadata_keys", metadataKeys(event.Metadata))
		c.JSON(http.StatusOK, gin.H{"status": "error", "message": "Unrecognised invoice metadata"})
		return
	}

	username, ok := event.Metadata[MetaUsername].(string)
	if !ok || username == "" {
		slog.Error("webhook missing username in metadata", "invoice_id", event.InvoiceID)
		c.JSON(http.StatusOK, gin.H{"status": "error", "message": "Missing username"})
		return
	}

	// The amount is what the buyer gets credited if they cannot have the
	// name. Without it a race loss would credit 0 and answer 200, keeping the
	// money; so a failed lookup is a 500 and BTCPay redelivers. Every failure
	// below that would leave a payer with neither the name nor the credit is a
	// 500 for the same reason, and the credit is once per invoice so a
	// redelivery cannot pay out twice.
	invoice, err := h.btcpay.GetInvoice(ctx, event.InvoiceID)
	if err != nil {
		slog.Error("failed to get invoice details", "invoice_id", event.InvoiceID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Invoice lookup failed"})
		return
	}
	amountSats := int64(invoice.Amount)

	// Attempt atomic registration (race-based: first payment wins)
	addr, err := h.store.AtomicRegisterAddress(ctx, username, h.cfg.Domain, pubkey, false)
	if err != nil {
		slog.Error("failed to register address",
			"username", username,
			"pubkey", pubkey,
			"invoice_id", event.InvoiceID,
			"error", err,
		)
		// Credit the user since we couldn't register
		if amountSats > 0 {
			if _, err := h.store.AddInvoiceCreditOnce(ctx, pubkey, amountSats, storage.CreditReasonRegistrationError, event.InvoiceID); err != nil {
				slog.Error("failed to credit user after error", "pubkey", pubkey, "invoice_id", event.InvoiceID, "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Credit failed"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "error", "message": "Registration failed, credited"})
		return
	}

	if addr == nil {
		// Username was taken (race loss) - credit the user
		slog.Info("username taken, crediting user",
			"username", username,
			"pubkey", pubkey,
			"amount_sats", amountSats,
			"invoice_id", event.InvoiceID,
		)
		if amountSats > 0 {
			credited, err := h.store.AddInvoiceCreditOnce(ctx, pubkey, amountSats, storage.CreditReasonRaceLoss, event.InvoiceID)
			if err != nil {
				slog.Error("failed to credit user after race loss", "pubkey", pubkey, "invoice_id", event.InvoiceID, "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Credit failed"})
				return
			}
			if !credited {
				slog.Info("race loss already credited for this invoice", "invoice_id", event.InvoiceID)
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"status":   "race_loss",
			"credited": amountSats,
			"message":  "Username taken, payment credited",
		})
		return
	}

	// Registration successful
	slog.Info("address registered via BTCPay",
		"username", username,
		"pubkey", pubkey,
		"invoice_id", event.InvoiceID,
		"amount_sats", amountSats,
	)

	// Auto-configure Lightning proxy if the invoice carried an address.
	if lnAddr, _ := event.Metadata[MetaLightningAddress].(string); lnAddr != "" {
		h.autoConfigureLightning(ctx, addr.ID, &lnAddr, username)
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "registered",
		"username": username,
		"address":  username + "@" + h.cfg.Domain,
	})
}

// metadataKeys lists the metadata key names for a log line, without the values —
// invoice metadata carries a pubkey and should not be dumped wholesale.
func metadataKeys(meta map[string]any) []string {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
