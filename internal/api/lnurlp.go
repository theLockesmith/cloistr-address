package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/metrics"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/nwc"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// LNURLPConfigResponse represents the LNURL-pay initial response
type LNURLPConfigResponse struct {
	Tag             string `json:"tag"`
	Callback        string `json:"callback"`
	MinSendable     int64  `json:"minSendable"`     // millisatoshis
	MaxSendable     int64  `json:"maxSendable"`     // millisatoshis
	Metadata        string `json:"metadata"`        // JSON array of [type, content]
	CommentAllowed  int    `json:"commentAllowed"`  // max comment length
	AllowsNostr     bool   `json:"allowsNostr"`     // NIP-57 zap support
	NostrPubkey     string `json:"nostrPubkey,omitempty"`
}

// LNURLPCallbackResponse represents the invoice response
type LNURLPCallbackResponse struct {
	PR            string          `json:"pr"`                      // BOLT11 invoice
	Routes        []interface{}   `json:"routes"`                  // routing hints
	SuccessAction *SuccessAction  `json:"successAction,omitempty"` // optional success action
}

// SuccessAction represents post-payment action
type SuccessAction struct {
	Tag     string `json:"tag"`
	Message string `json:"message,omitempty"`
	URL     string `json:"url,omitempty"`
}

// LNURLErrorResponse represents an error response
type LNURLErrorResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// handleLNURLPConfig handles the initial LNURL-pay request
// GET /.well-known/lnurlp/:username
func (h *Handler) handleLNURLPConfig(c *gin.Context) {
	username := c.Param("username")
	ctx := c.Request.Context()

	// Look up the address
	addr, err := h.store.GetAddressByUsername(ctx, username, h.cfg.Domain)
	if err != nil {
		slog.Error("failed to get address for LNURLP", "username", username, "error", err)
		metrics.LNURLRequests.WithLabelValues("config", "error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Service error",
		})
		return
	}

	if addr == nil {
		metrics.LNURLRequests.WithLabelValues("config", "not_found").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "User not found",
		})
		return
	}

	// Get Lightning configuration
	lnConfig, err := h.store.GetLightningConfig(ctx, addr.ID)
	if err != nil {
		slog.Error("failed to get lightning config", "username", username, "error", err)
		metrics.LNURLRequests.WithLabelValues("config", "error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Service error",
		})
		return
	}

	// Check if Lightning is enabled
	if lnConfig == nil || !lnConfig.Enabled || lnConfig.Mode == "disabled" {
		metrics.LNURLRequests.WithLabelValues("config", "disabled").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Lightning Address not configured for this user",
		})
		return
	}

	// Build callback URL
	scheme := "https"
	if c.Request.TLS == nil && strings.HasPrefix(c.Request.Host, "localhost") {
		scheme = "http"
	}
	callbackURL := fmt.Sprintf("%s://%s/.well-known/lnurlp/%s/callback",
		scheme, c.Request.Host, username)

	// Build metadata (required by LNURL spec)
	metadata := fmt.Sprintf("[[\"text/identifier\",\"%s@%s\"]]", username, h.cfg.Domain)

	response := LNURLPConfigResponse{
		Tag:            "payRequest",
		Callback:       callbackURL,
		MinSendable:    lnConfig.MinSendableMsats,
		MaxSendable:    lnConfig.MaxSendableMsats,
		Metadata:       metadata,
		CommentAllowed: lnConfig.CommentAllowed,
		AllowsNostr:    lnConfig.AllowsNostr,
		NostrPubkey:    addr.Pubkey,
	}

	metrics.LNURLRequests.WithLabelValues("config", "success").Inc()
	c.JSON(http.StatusOK, response)
}

// handleLNURLPCallback handles invoice generation requests
// GET /.well-known/lnurlp/:username/callback?amount=1000&comment=test
func (h *Handler) handleLNURLPCallback(c *gin.Context) {
	username := c.Param("username")
	ctx := c.Request.Context()

	// Parse amount (required, in millisatoshis)
	amountStr := c.Query("amount")
	if amountStr == "" {
		metrics.LNURLRequests.WithLabelValues("callback", "bad_request").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Missing amount parameter",
		})
		return
	}

	var amount int64
	if _, err := fmt.Sscanf(amountStr, "%d", &amount); err != nil {
		metrics.LNURLRequests.WithLabelValues("callback", "bad_request").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Invalid amount",
		})
		return
	}

	comment := c.Query("comment")

	// Look up the address
	addr, err := h.store.GetAddressByUsername(ctx, username, h.cfg.Domain)
	if err != nil {
		slog.Error("failed to get address for LNURLP callback", "username", username, "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Service error",
		})
		return
	}

	if addr == nil {
		metrics.LNURLRequests.WithLabelValues("callback", "not_found").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "User not found",
		})
		return
	}

	// Get Lightning configuration
	lnConfig, err := h.store.GetLightningConfig(ctx, addr.ID)
	if err != nil || lnConfig == nil || !lnConfig.Enabled {
		metrics.LNURLRequests.WithLabelValues("callback", "disabled").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Lightning Address not configured",
		})
		return
	}

	// Validate amount
	if amount < lnConfig.MinSendableMsats || amount > lnConfig.MaxSendableMsats {
		metrics.LNURLRequests.WithLabelValues("callback", "bad_request").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: fmt.Sprintf("Amount must be between %d and %d millisatoshis",
				lnConfig.MinSendableMsats, lnConfig.MaxSendableMsats),
		})
		return
	}

	// Handle based on mode
	switch lnConfig.Mode {
	case "proxy":
		h.handleProxyInvoice(c, lnConfig.ProxyAddress, amount, comment)
	case "nwc":
		h.handleNWCInvoice(c, ctx, lnConfig, amount, comment, username)
	case "hosted":
		h.handleHostedInvoice(c)
	default:
		metrics.LNURLRequests.WithLabelValues("callback", "disabled").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Lightning Address not configured",
		})
	}
}

// handleProxyInvoice fetches an invoice from the user's configured Lightning Address
func (h *Handler) handleProxyInvoice(c *gin.Context, proxyAddress string, amount int64, comment string) {
	// Parse the proxy address (e.g., "alice@getalby.com")
	parts := strings.SplitN(proxyAddress, "@", 2)
	if len(parts) != 2 {
		slog.Error("invalid proxy address format", "address", proxyAddress)
		metrics.LNURLRequests.WithLabelValues("callback", "proxy_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Invalid proxy configuration",
		})
		return
	}

	proxyUsername := parts[0]
	proxyDomain := parts[1]

	// Step 1: Fetch LNURLP config from proxy
	configURL := fmt.Sprintf("https://%s/.well-known/lnurlp/%s", proxyDomain, proxyUsername)

	resp, err := http.Get(configURL)
	if err != nil {
		slog.Error("failed to fetch proxy LNURLP config", "url", configURL, "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "proxy_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Failed to reach payment provider",
		})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("failed to read proxy response", "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "proxy_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Failed to reach payment provider",
		})
		return
	}

	var proxyConfig LNURLPConfigResponse
	if err := json.Unmarshal(body, &proxyConfig); err != nil {
		slog.Error("failed to parse proxy config", "error", err, "body", string(body))
		metrics.LNURLRequests.WithLabelValues("callback", "proxy_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Invalid response from payment provider",
		})
		return
	}

	// Step 2: Request invoice from proxy callback
	callbackURL, err := url.Parse(proxyConfig.Callback)
	if err != nil {
		slog.Error("invalid proxy callback URL", "url", proxyConfig.Callback, "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "proxy_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Invalid response from payment provider",
		})
		return
	}

	q := callbackURL.Query()
	q.Set("amount", fmt.Sprintf("%d", amount))
	if comment != "" && proxyConfig.CommentAllowed > 0 {
		// Truncate comment if needed
		if len(comment) > proxyConfig.CommentAllowed {
			comment = comment[:proxyConfig.CommentAllowed]
		}
		q.Set("comment", comment)
	}
	callbackURL.RawQuery = q.Encode()

	invoiceResp, err := http.Get(callbackURL.String())
	if err != nil {
		slog.Error("failed to fetch proxy invoice", "url", callbackURL.String(), "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "proxy_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Failed to generate invoice",
		})
		return
	}
	defer invoiceResp.Body.Close()

	invoiceBody, err := io.ReadAll(invoiceResp.Body)
	if err != nil {
		slog.Error("failed to read invoice response", "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "proxy_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Failed to generate invoice",
		})
		return
	}

	// Check for error response
	var errorCheck struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(invoiceBody, &errorCheck); err == nil && errorCheck.Status == "ERROR" {
		slog.Warn("proxy returned error", "reason", errorCheck.Reason)
		metrics.LNURLRequests.WithLabelValues("callback", "proxy_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: errorCheck.Reason,
		})
		return
	}

	// Parse invoice response
	var invoiceData LNURLPCallbackResponse
	if err := json.Unmarshal(invoiceBody, &invoiceData); err != nil {
		slog.Error("failed to parse invoice response", "error", err, "body", string(invoiceBody))
		metrics.LNURLRequests.WithLabelValues("callback", "proxy_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Failed to generate invoice",
		})
		return
	}

	// Return the invoice
	metrics.LNURLRequests.WithLabelValues("callback", "success").Inc()
	c.JSON(http.StatusOK, invoiceData)
}

// handleNWCInvoice generates an invoice using Nostr Wallet Connect
func (h *Handler) handleNWCInvoice(c *gin.Context, ctx context.Context, lnConfig *storage.AddressLightning, amount int64, comment string, username string) {
	// Check if NWC encryption is configured
	if h.nwcEncryptor == nil {
		slog.Error("NWC encryptor not configured")
		metrics.LNURLRequests.WithLabelValues("callback", "nwc_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "NWC not configured on server",
		})
		return
	}

	// Check if NWC connection details are available
	if lnConfig.NWCSecretEncrypted == "" || lnConfig.NWCRelayURL == "" || lnConfig.NWCWalletPubkey == "" {
		slog.Error("NWC connection details missing", "username", username)
		metrics.LNURLRequests.WithLabelValues("callback", "nwc_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "NWC not configured for this user",
		})
		return
	}

	// Decrypt the NWC secret
	nwcSecret, err := h.nwcEncryptor.Decrypt(lnConfig.NWCSecretEncrypted)
	if err != nil {
		slog.Error("failed to decrypt NWC secret", "username", username, "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "nwc_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Failed to process NWC configuration",
		})
		return
	}

	// Create NWC client
	nwcConfig := nwc.ConnectionConfig{
		WalletPubkey: lnConfig.NWCWalletPubkey,
		RelayURL:     lnConfig.NWCRelayURL,
		Secret:       nwcSecret,
	}

	client, err := nwc.NewClient(nwcConfig)
	if err != nil {
		slog.Error("failed to create NWC client", "username", username, "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "nwc_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Failed to connect to wallet",
		})
		return
	}

	// Connect to relay
	if err := client.Connect(ctx); err != nil {
		slog.Error("failed to connect to NWC relay", "username", username, "relay", lnConfig.NWCRelayURL, "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "nwc_error").Inc()
		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Failed to connect to wallet relay",
		})
		return
	}
	defer client.Close()

	// Build description
	description := fmt.Sprintf("Payment to %s@%s", username, h.cfg.Domain)
	if comment != "" {
		description = fmt.Sprintf("%s: %s", description, comment)
	}

	// Request invoice
	invoiceResp, err := client.MakeInvoice(ctx, nwc.MakeInvoiceRequest{
		Amount:      amount,
		Description: description,
		Expiry:      3600, // 1 hour expiry
	})
	if err != nil {
		slog.Error("NWC make_invoice failed", "username", username, "error", err)
		metrics.LNURLRequests.WithLabelValues("callback", "nwc_error").Inc()

		// Update error tracking in database
		h.store.UpdateNWCError(ctx, lnConfig.AddressID, err.Error())

		c.JSON(http.StatusOK, LNURLErrorResponse{
			Status: "ERROR",
			Reason: "Failed to generate invoice from wallet",
		})
		return
	}

	// Update success tracking
	h.store.UpdateNWCSuccess(ctx, lnConfig.AddressID)

	slog.Info("NWC invoice generated",
		"username", username,
		"amount_msat", amount,
		"payment_hash", invoiceResp.PaymentHash[:8]+"...",
	)

	metrics.LNURLRequests.WithLabelValues("callback", "nwc_success").Inc()
	c.JSON(http.StatusOK, LNURLPCallbackResponse{
		PR:     invoiceResp.Invoice,
		Routes: []interface{}{},
	})
}

// handleHostedInvoice handles hosted Lightning mode (stub for future implementation)
func (h *Handler) handleHostedInvoice(c *gin.Context) {
	metrics.LNURLRequests.WithLabelValues("callback", "hosted_not_ready").Inc()
	c.JSON(http.StatusOK, LNURLErrorResponse{
		Status: "ERROR",
		Reason: "Hosted Lightning wallets coming soon",
	})
}
