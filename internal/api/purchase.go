package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	nameval "git.aegis-hq.xyz/coldforge/cloistr-common/username"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// PurchaseQuoteRequest represents a quote request
type PurchaseQuoteRequest struct {
	Username string `json:"username" binding:"required"`
}

// PurchaseQuoteResponse represents a purchase quote
type PurchaseQuoteResponse struct {
	Username  string `json:"username"`
	Available bool   `json:"available"`
	// NO omitempty on the money fields. A free-tier name is priced at 0, and
	// omitempty drops a 0 int from the JSON entirely -- so the client received
	// no price_sats at all and rendered `quote.price_sats?.toLocaleString()`
	// as an empty string, showing a bare "sats" with a 0 total. A price of zero
	// is a real answer and has to be transmitted as one; "absent" and "free"
	// are different states and the UI cannot distinguish them otherwise.
	// Credits gets the same treatment: 0 credits is meaningful, not missing.
	PriceSats int64  `json:"price_sats"`
	Tier      string `json:"tier,omitempty"`
	Credits   int64  `json:"credits"` // User's available credits
	// Additional is true when this account has already claimed its one free
	// name, so a normally-free name is being charged the additional-address
	// price. The UI needs this to explain the charge; without it the user just
	// sees a number where they expected "Free".
	Additional bool `json:"additional"`
}

// PurchaseInvoiceRequest represents an invoice creation request
type PurchaseInvoiceRequest struct {
	Username   string `json:"username" binding:"required"`
	UseCredits bool   `json:"use_credits,omitempty"` // Apply credits to reduce price
}

// PurchaseInvoiceResponse represents a created invoice
type PurchaseInvoiceResponse struct {
	InvoiceID      string `json:"invoice_id"`
	Username       string `json:"username"`
	AmountSats     int64  `json:"amount_sats"`
	CreditsApplied int64  `json:"credits_applied,omitempty"`
	PaymentRequest string `json:"payment_request,omitempty"` // BOLT11 invoice
	ExpiresAt      string `json:"expires_at"`
}

// CreditBalanceResponse represents user's credit balance
type CreditBalanceResponse struct {
	BalanceSats int64 `json:"balance_sats"`
}

// CreditWithdrawRequest represents a withdrawal request
type CreditWithdrawRequest struct {
	AmountSats       int64  `json:"amount_sats" binding:"required,min=1"`
	LightningAddress string `json:"lightning_address" binding:"required"`
}

// CreditWithdrawResponse represents a withdrawal response
type CreditWithdrawResponse struct {
	WithdrawalID int64  `json:"withdrawal_id"`
	AmountSats   int64  `json:"amount_sats"`
	Status       string `json:"status"`
	Message      string `json:"message"`
}

// getPurchaseQuote returns a quote for purchasing a username
// POST /api/v1/purchase/quote
func (h *Handler) getPurchaseQuote(c *gin.Context) {
	ctx := c.Request.Context()
	pubkey := auth.GetPubkey(c)

	var req PurchaseQuoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	username := req.Username

	// Validate format
	if !nameval.IsValidHumanName(username) {
		c.JSON(http.StatusOK, PurchaseQuoteResponse{
			Username:  username,
			Available: false,
		})
		return
	}

	// Check availability
	available, err := h.store.IsUsernameAvailable(ctx, username)
	if err != nil {
		slog.Error("failed to check username", "username", username, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	response := PurchaseQuoteResponse{
		Username:  username,
		Available: available,
	}

	if available {
		// Priced FOR THIS PUBKEY: one free real name per account, so a free-tier
		// name costs the additional-address price once the account has claimed
		// one. A swallowed lookup error would quote price_sats: 0, which is
		// indistinguishable from a genuinely free name and sends the user to a
		// "Claim Free" button that cannot work.
		priced, err := h.priceNameFor(ctx, username, pubkey)
		if err != nil {
			slog.Error("failed to price name", "username", username, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
			return
		}
		response.PriceSats = priced.PriceSats
		response.Tier = priced.Tier
		response.Additional = priced.Additional

		// Credits are additive: failing to read them understates the discount but
		// never overstates what the user owes, so this one stays non-fatal.
		credits, err := h.store.GetCredits(ctx, pubkey)
		if err != nil {
			slog.Error("failed to get credits", "error", err)
		}
		response.Credits = credits
	}

	c.JSON(http.StatusOK, response)
}

// createPurchaseInvoice creates an invoice for purchasing a username
// Race-based: First payment to complete wins the registration
// POST /api/v1/purchase/invoice
func (h *Handler) createPurchaseInvoice(c *gin.Context) {
	ctx := c.Request.Context()
	pubkey := auth.GetPubkey(c)

	var req PurchaseInvoiceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	username := req.Username

	// Validate format
	if !nameval.IsValidHumanName(username) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid username format"})
		return
	}

	// Check availability (informational - race may still occur)
	available, err := h.store.IsUsernameAvailable(ctx, username)
	if err != nil {
		slog.Error("failed to check username", "username", username, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}
	if !available {
		c.JSON(http.StatusConflict, gin.H{"error": "Username not available"})
		return
	}

	// Price for THIS pubkey, matching the quote: the first real name on an
	// account is free, later ones are not. Charging off the length-only tier
	// here would hand out unlimited free names no matter what the quote said.
	priced, err := h.priceNameFor(ctx, username, pubkey)
	if err != nil {
		slog.Error("failed to price name", "username", username, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}
	price := priced.PriceSats

	// Apply credits if requested
	var creditsApplied int64
	if req.UseCredits {
		credits, err := h.store.GetCredits(ctx, pubkey)
		if err != nil {
			slog.Error("failed to get credits", "error", err)
		} else if credits > 0 {
			if credits >= price {
				creditsApplied = price
			} else {
				creditsApplied = credits
			}
		}
	}

	finalPrice := price - creditsApplied

	// If fully covered by credits (or free), register immediately.
	// creditsApplied may be zero here for a free-tier name; only call DeductCredits
	// when there is actually something to deduct — an UPDATE…RETURNING with amount=0
	// against a pubkey that has no pubkey_credits row returns sql.ErrNoRows, which
	// DeductCredits maps to ErrInsufficientCredits, incorrectly blocking registration.
	if finalPrice == 0 {
		if creditsApplied > 0 {
			// Deduct credits before registering so the balance can't be double-spent.
			err = h.store.DeductCredits(ctx, pubkey, creditsApplied, "purchase_full", username)
			if err != nil {
				if errors.Is(err, storage.ErrInsufficientCredits) {
					credits, _ := h.store.GetCredits(ctx, pubkey)
					c.JSON(http.StatusBadRequest, gin.H{
						"error":          "Insufficient credits",
						"available_sats": credits,
						"required_sats":  creditsApplied,
					})
					return
				}
				slog.Error("failed to deduct credits", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
				return
			}
		}

		// Attempt registration
		addr, err := h.store.AtomicRegisterAddress(ctx, username, h.cfg.Domain, pubkey, false)
		if err != nil {
			if creditsApplied > 0 {
				// Refund credits only if we actually deducted some. A failed
				// refund silently costs the user sats, so it is logged at ERROR
				// with everything needed to replay it by hand.
				refund(ctx, h, pubkey, creditsApplied, "purchase_failed_refund", username)
			}
			slog.Error("failed to register address", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
			return
		}
		if addr == nil {
			// Username was taken in a race; refund any credits that were deducted.
			refunded := true
			if creditsApplied > 0 {
				refunded = refund(ctx, h, pubkey, creditsApplied, "username_taken_refund", username)
			}
			resp := gin.H{"error": "Username was taken"}
			if creditsApplied > 0 {
				// Only claim the refund when it actually landed. Telling a user
				// their credits are back when the write failed is worse than
				// telling them nothing.
				if refunded {
					resp["message"] = "Credits have been refunded to your account"
				} else {
					resp["message"] = "Your credits could not be refunded automatically — please contact support"
				}
			}
			c.JSON(http.StatusConflict, resp)
			return
		}

		slog.Info("registered address via credits",
			"username", username,
			"pubkey", pubkey,
			"credits_used", creditsApplied,
		)

		c.JSON(http.StatusCreated, gin.H{
			"success":        true,
			"username":       username,
			"credits_used":   creditsApplied,
			"message":        "Address registered successfully",
			"payment_method": "credits",
		})
		return
	}

	// Deduct partial credits now if any
	if creditsApplied > 0 {
		err = h.store.DeductCredits(ctx, pubkey, creditsApplied, "purchase_partial", username)
		if err != nil {
			slog.Error("failed to deduct partial credits", "pubkey", pubkey, "amount", creditsApplied, "error", err)
			// Continue anyway - don't fail the invoice creation
			creditsApplied = 0
			finalPrice = price
		}
	}

	// Check if BTCPay is configured
	if !h.btcpay.IsConfigured() {
		slog.Error("BTCPay not configured")
		// Refund any deducted credits
		if creditsApplied > 0 {
			refund(ctx, h, pubkey, creditsApplied, "btcpay_unavailable_refund", username)
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Payment system unavailable"})
		return
	}

	// Create BTCPay invoice with metadata for webhook processing
	metadata := map[string]interface{}{
		// Tagged explicitly. settlementKind() still infers "address" from a bare
		// username so invoices minted before this key existed keep settling.
		MetaKind:          KindAddress,
		MetaUsername:      username,
		MetaPubkey:        pubkey,
		"credits_applied": creditsApplied,
		"original_price":  price,
	}

	invoice, err := h.btcpay.CreateInvoice(finalPrice, metadata)
	if err != nil {
		slog.Error("failed to create BTCPay invoice",
			"username", username,
			"pubkey", pubkey,
			"amount", finalPrice,
			"error", err,
		)
		// Refund any deducted credits
		if creditsApplied > 0 {
			refund(ctx, h, pubkey, creditsApplied, "invoice_creation_failed_refund", username)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create invoice"})
		return
	}

	// Get payment methods to retrieve BOLT11 invoice
	var paymentRequest string
	methods, err := h.btcpay.GetInvoicePaymentMethods(invoice.ID)
	if err != nil {
		slog.Warn("failed to get payment methods", "invoice_id", invoice.ID, "error", err)
	} else {
		for _, m := range methods {
			if m.PaymentMethod == "BTC-LightningNetwork" {
				paymentRequest = m.Destination
				break
			}
		}
	}

	slog.Info("created BTCPay invoice",
		"username", username,
		"pubkey", pubkey,
		"amount_sats", finalPrice,
		"credits_applied", creditsApplied,
		"invoice_id", invoice.ID,
	)

	response := PurchaseInvoiceResponse{
		InvoiceID:      invoice.ID,
		Username:       username,
		AmountSats:     finalPrice,
		CreditsApplied: creditsApplied,
		PaymentRequest: paymentRequest,
		ExpiresAt:      time.Unix(invoice.ExpirationTime, 0).Format(time.RFC3339),
	}

	c.JSON(http.StatusCreated, response)
}

// getCredits returns the user's credit balance
// GET /api/v1/credits
func (h *Handler) getCredits(c *gin.Context) {
	ctx := c.Request.Context()
	pubkey := auth.GetPubkey(c)

	credits, err := h.store.GetCredits(ctx, pubkey)
	if err != nil {
		slog.Error("failed to get credits", "pubkey", pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	c.JSON(http.StatusOK, CreditBalanceResponse{
		BalanceSats: credits,
	})
}

// withdrawCredits initiates a credit withdrawal to a Lightning address
// POST /api/v1/credits/withdraw
func (h *Handler) withdrawCredits(c *gin.Context) {
	ctx := c.Request.Context()
	pubkey := auth.GetPubkey(c)

	var req CreditWithdrawRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Validate Lightning address format
	if !isValidLightningAddress(req.LightningAddress) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid Lightning address format"})
		return
	}

	// Check minimum withdrawal (must cover potential routing fees)
	const minWithdrawal = 100 // 100 sats minimum
	if req.AmountSats < minWithdrawal {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Withdrawal amount too small",
			"minimum": minWithdrawal,
		})
		return
	}

	// Create withdrawal request (atomically deducts credits)
	withdrawal, err := h.store.CreateWithdrawalRequest(ctx, pubkey, req.AmountSats, req.LightningAddress)
	if err != nil {
		if errors.Is(err, storage.ErrInsufficientCredits) {
			credits, _ := h.store.GetCredits(ctx, pubkey)
			c.JSON(http.StatusBadRequest, gin.H{
				"error":          "Insufficient credits",
				"available_sats": credits,
				"requested_sats": req.AmountSats,
			})
			return
		}
		slog.Error("failed to create withdrawal", "pubkey", pubkey, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

	slog.Info("created withdrawal request",
		"withdrawal_id", withdrawal.ID,
		"pubkey", pubkey,
		"amount_sats", req.AmountSats,
		"lightning_address", req.LightningAddress,
	)

	// TODO: Queue withdrawal for processing via LND
	// For now, the withdrawal is in "pending" state

	c.JSON(http.StatusAccepted, CreditWithdrawResponse{
		WithdrawalID: withdrawal.ID,
		AmountSats:   withdrawal.AmountSats,
		Status:       withdrawal.Status,
		Message:      "Withdrawal request created. Payment will be processed shortly.",
	})
}

// refund returns credits that were deducted for a purchase that then failed.
//
// It reports whether the credits actually went back. Every call site used to
// ignore the error, so a failed refund left the user short with nothing in the
// logs to reconcile from — and one of them told the user "Credits have been
// refunded" regardless. The ERROR line carries enough to replay the credit by
// hand.
func refund(ctx context.Context, h *Handler, pubkey string, amount int64, reason, username string) bool {
	if err := h.store.AddCredits(ctx, pubkey, amount, reason, username); err != nil {
		slog.Error("REFUND FAILED - credits owed to user",
			"pubkey", pubkey,
			"amount_sats", amount,
			"reason", reason,
			"username", username,
			"error", err,
		)
		return false
	}
	return true
}
