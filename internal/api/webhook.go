package api

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"git.coldforge.xyz/coldforge/cloistr-me/internal/btcpay"
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

	// Extract metadata
	username, ok := event.Metadata["username"].(string)
	if !ok || username == "" {
		slog.Error("webhook missing username in metadata", "invoice_id", event.InvoiceID)
		c.JSON(http.StatusOK, gin.H{"status": "error", "message": "Missing username"})
		return
	}

	pubkey, ok := event.Metadata["pubkey"].(string)
	if !ok || pubkey == "" {
		slog.Error("webhook missing pubkey in metadata", "invoice_id", event.InvoiceID)
		c.JSON(http.StatusOK, gin.H{"status": "error", "message": "Missing pubkey"})
		return
	}

	// Get invoice details for amount (in case of overpayment credits)
	invoice, err := h.btcpay.GetInvoice(event.InvoiceID)
	if err != nil {
		slog.Error("failed to get invoice details", "invoice_id", event.InvoiceID, "error", err)
		// Continue anyway - we'll use 0 for amount if needed
	}

	var amountSats int64
	if invoice != nil {
		amountSats = int64(invoice.Amount)
	}

	// Attempt atomic registration (race-based: first payment wins)
	addr, err := h.store.AtomicRegisterAddress(ctx, username, h.cfg.Domain, pubkey)
	if err != nil {
		slog.Error("failed to register address",
			"username", username,
			"pubkey", pubkey,
			"invoice_id", event.InvoiceID,
			"error", err,
		)
		// Credit the user since we couldn't register
		if amountSats > 0 {
			if err := h.store.AddCredits(ctx, pubkey, amountSats, "registration_error", event.InvoiceID); err != nil {
				slog.Error("failed to credit user after error", "pubkey", pubkey, "error", err)
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
			if err := h.store.AddCredits(ctx, pubkey, amountSats, "race_loss", event.InvoiceID); err != nil {
				slog.Error("failed to credit user after race loss", "pubkey", pubkey, "error", err)
				c.JSON(http.StatusOK, gin.H{"status": "error", "message": "Credit failed"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"status":     "race_loss",
			"credited":   amountSats,
			"message":    "Username taken, payment credited",
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

	c.JSON(http.StatusOK, gin.H{
		"status":   "registered",
		"username": username,
		"address":  username + "@" + h.cfg.Domain,
	})
}
