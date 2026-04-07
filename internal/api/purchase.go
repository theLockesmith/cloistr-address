package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"git.coldforge.xyz/coldforge/cloistr-address/internal/auth"
	"git.coldforge.xyz/coldforge/cloistr-address/internal/storage"
)

// PurchaseQuoteRequest represents a quote request
type PurchaseQuoteRequest struct {
	Username string `json:"username" binding:"required"`
}

// PurchaseQuoteResponse represents a purchase quote
type PurchaseQuoteResponse struct {
	Username  string `json:"username"`
	Available bool   `json:"available"`
	PriceSats int64  `json:"price_sats,omitempty"`
	Tier      string `json:"tier,omitempty"`
	Credits   int64  `json:"credits,omitempty"` // User's available credits
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
	if !usernameRegex.MatchString(username) {
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
		// Get pricing
		price, err := h.store.GetUsernamePrice(ctx, len(username))
		if err != nil {
			slog.Error("failed to get price", "error", err)
		} else {
			response.PriceSats = price
		}

		tier, err := h.store.GetUsernameTier(ctx, len(username))
		if err != nil {
			slog.Error("failed to get tier", "error", err)
		} else {
			response.Tier = tier
		}

		// Get user's credits
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
	if !usernameRegex.MatchString(username) {
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

	// Get price
	price, err := h.store.GetUsernamePrice(ctx, len(username))
	if err != nil {
		slog.Error("failed to get price", "username", username, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
		return
	}

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

	// If fully covered by credits, register immediately
	if finalPrice == 0 {
		// Deduct credits first
		err = h.store.DeductCredits(ctx, pubkey, creditsApplied, "purchase_full", username)
		if err != nil {
			if errors.Is(err, storage.ErrInsufficientCredits) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Insufficient credits"})
				return
			}
			slog.Error("failed to deduct credits", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
			return
		}

		// Attempt registration
		addr, err := h.store.AtomicRegisterAddress(ctx, username, h.cfg.Domain, pubkey)
		if err != nil {
			// Refund credits on error
			h.store.AddCredits(ctx, pubkey, creditsApplied, "purchase_failed_refund", username)
			slog.Error("failed to register address", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Service error"})
			return
		}
		if addr == nil {
			// Username was taken, refund credits
			h.store.AddCredits(ctx, pubkey, creditsApplied, "username_taken_refund", username)
			c.JSON(http.StatusConflict, gin.H{
				"error":   "Username was taken",
				"message": "Credits have been refunded to your account",
			})
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

	// TODO: Create BTCPay invoice here
	// For now, return a placeholder response
	invoiceID := generateInvoiceID(username, pubkey)
	expiresAt := time.Now().Add(1 * time.Hour) // 1 hour expiry

	slog.Info("created purchase invoice",
		"username", username,
		"pubkey", pubkey,
		"amount_sats", finalPrice,
		"credits_applied", creditsApplied,
		"invoice_id", invoiceID,
	)

	response := PurchaseInvoiceResponse{
		InvoiceID:      invoiceID,
		Username:       username,
		AmountSats:     finalPrice,
		CreditsApplied: creditsApplied,
		ExpiresAt:      expiresAt.Format(time.RFC3339),
		// PaymentRequest will be filled when BTCPay is integrated
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
				"error":           "Insufficient credits",
				"available_sats":  credits,
				"requested_sats":  req.AmountSats,
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

// generateInvoiceID creates a unique invoice ID
// Format: cloistr_<timestamp>_<username_prefix>
func generateInvoiceID(username, pubkey string) string {
	return "cloistr_" + time.Now().Format("20060102150405") + "_" + username[:min(8, len(username))]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

