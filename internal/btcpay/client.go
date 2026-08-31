package btcpay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
)

// Client handles BTCPay Server API interactions
type Client struct {
	baseURL       string
	apiKey        string
	storeID       string
	webhookSecret string
	httpClient    *http.Client

	// Timeouts live on the client so tests can shrink them. Production values
	// come from the constants below; nothing else overrides them.
	budget         time.Duration
	attemptTimeout time.Duration
	retryWait      time.Duration
}

// InvoiceRequest represents a request to create an invoice
type InvoiceRequest struct {
	Amount   int64                  `json:"amount"`
	Currency string                 `json:"currency"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
	Checkout *InvoiceCheckout       `json:"checkout,omitempty"`
}

// InvoiceCheckout configures checkout behavior
type InvoiceCheckout struct {
	SpeedPolicy       string   `json:"speedPolicy,omitempty"`       // HighSpeed, MediumSpeed, LowMediumSpeed, LowSpeed
	PaymentMethods    []string `json:"paymentMethods,omitempty"`    // e.g., ["BTC-LightningNetwork"]
	ExpirationMinutes int      `json:"expirationMinutes,omitempty"` // Default 15
	RedirectURL       string   `json:"redirectURL,omitempty"`
}

// Invoice represents a BTCPay invoice response
type Invoice struct {
	ID                   string                 `json:"id"`
	StoreID              string                 `json:"storeId"`
	Amount               float64                `json:"amount,string"`
	Currency             string                 `json:"currency"`
	Status               string                 `json:"status"` // New, Processing, Expired, Invalid, Settled
	AdditionalStatus     string                 `json:"additionalStatus"`
	CreatedTime          int64                  `json:"createdTime"`
	ExpirationTime       int64                  `json:"expirationTime"`
	MonitoringExpiration int64                  `json:"monitoringExpiration"`
	CheckoutLink         string                 `json:"checkoutLink"`
	Metadata             map[string]interface{} `json:"metadata"`
}

// InvoicePaymentMethod represents payment details for an invoice
type InvoicePaymentMethod struct {
	PaymentMethod     string `json:"paymentMethod"`
	Destination       string `json:"destination"` // BOLT11 invoice for Lightning
	PaymentLink       string `json:"paymentLink"`
	Rate              string `json:"rate"`
	PaymentMethodPaid string `json:"paymentMethodPaid"`
	TotalPaid         string `json:"totalPaid"`
	Due               string `json:"due"`
	Amount            string `json:"amount"`
}

// WebhookEvent represents a webhook notification from BTCPay
type WebhookEvent struct {
	DeliveryID         string                 `json:"deliveryId"`
	WebhookID          string                 `json:"webhookId"`
	OriginalDeliveryID string                 `json:"originalDeliveryId,omitempty"`
	IsRedelivery       bool                   `json:"isRedelivery"`
	Type               string                 `json:"type"` // InvoiceCreated, InvoiceReceivedPayment, InvoiceProcessing, InvoiceExpired, InvoiceSettled, InvoiceInvalid
	Timestamp          int64                  `json:"timestamp"`
	StoreID            string                 `json:"storeId"`
	InvoiceID          string                 `json:"invoiceId"`
	Metadata           map[string]interface{} `json:"metadata,omitempty"`
	ManuallyMarked     bool                   `json:"manuallyMarked,omitempty"`
	OverPaid           bool                   `json:"overPaid,omitempty"`
}

// Invoice status constants
const (
	StatusNew        = "New"
	StatusProcessing = "Processing"
	StatusExpired    = "Expired"
	StatusInvalid    = "Invalid"
	StatusSettled    = "Settled"
)

// Webhook event types
const (
	EventInvoiceCreated         = "InvoiceCreated"
	EventInvoiceReceivedPayment = "InvoiceReceivedPayment"
	EventInvoiceProcessing      = "InvoiceProcessing"
	EventInvoiceExpired         = "InvoiceExpired"
	EventInvoiceSettled         = "InvoiceSettled"
	EventInvoiceInvalid         = "InvoiceInvalid"
)

// NewClient creates a new BTCPay client
func NewClient(cfg config.BTCPayConfig) *Client {
	return &Client{
		baseURL:       strings.TrimSuffix(cfg.URL, "/"),
		apiKey:        cfg.APIKey,
		storeID:       cfg.StoreID,
		webhookSecret: cfg.WebhookSecret,
		httpClient: &http.Client{
			// Kept as a backstop only. The real bound is the per-attempt
			// context below, which is what makes a slow BTCPay fail inside the
			// edge proxy's patience rather than outside it.
			Timeout: 30 * time.Second,
		},
		budget:         invoiceBudget,
		attemptTimeout: perAttemptTimeout,
		retryWait:      transientRetryWait,
	}
}

// IsConfigured returns true if BTCPay is configured
func (c *Client) IsConfigured() bool {
	return c.baseURL != "" && c.apiKey != "" && c.storeID != ""
}

// Timeout budget for talking to BTCPay.
//
// # WHY THESE NUMBERS
//
// The old client had a single 30s http.Client timeout and no deadline of its
// own. That is LONGER than the edge proxy in front of this service is willing
// to wait, and the consequence was measured in production: a second-name
// purchase took 24.5s, this service logged a perfectly good HTTP 201 carrying a
// BOLT11 — and the user got an opaque "502 Bad Gateway", because the proxy had
// already given up. The invoice existed and nobody could pay it.
//
// An inner timeout longer than the outer one can only ever produce a gateway
// error instead of an error this service is able to explain. So the whole
// sequence is bounded well inside the edge budget, and we answer in our own
// words when it does not fit.
//
// Sizing: a healthy create measured 1.2s–7.7s against the production store, and
// BTCPay's own Lightning reply budget is 5s. perAttemptTimeout is generous for
// the happy path; invoiceBudget leaves room for one retry plus the handler.
const (
	invoiceBudget      = 15 * time.Second
	perAttemptTimeout  = 7 * time.Second
	transientRetryWait = 500 * time.Millisecond

	// Hard cap on attempts, independent of the clock.
	//
	// A time-only guard is not enough: when BTCPay rejects FAST the loop fits
	// dozens of attempts inside the budget. A test measured 30 in 200ms. That
	// is a retry storm aimed at a Lightning node that is already struggling —
	// precisely the wrong response to the failure we are retrying.
	maxInvoiceAttempts = 3
)

// ErrPaymentMethodUnavailable is BTCPay refusing to build the Lightning payment
// method because LND did not answer in time.
//
// Reported as HTTP 400 with a body naming the node timeout:
//
//	BTC-LN: NodeInfo failed to be fetched: The lightning node did not reply in
//	a timely manner
//
// This is TRANSIENT and, critically, NON-DESTRUCTIVE. Measured against the
// production store: ten creates yielding nine successes and one such 400 left
// exactly nine invoices behind. The failed attempt creates NOTHING, and that is
// what makes retrying it safe — a retry cannot leave a user holding duplicate
// invoices.
var ErrPaymentMethodUnavailable = errors.New("btcpay: lightning payment method unavailable")

// isTransientLNFailure reports whether a BTCPay error response is the Lightning
// timeout above rather than a real rejection.
//
// Deliberately narrow. A blanket "retry any 400" would replay genuinely invalid
// requests, and would also retry failures that DO leave an invoice behind.
func isTransientLNFailure(status int, body string) bool {
	if status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(body, "did not reply in a timely manner") ||
		strings.Contains(body, "Payment method unavailable")
}

// send performs one BTCPay request within the caller's deadline.
func (c *Client) send(ctx context.Context, method, url string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Authorization", "token "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("failed to read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// CreateInvoice creates a new invoice for the specified amount in satoshis.
//
// Retries ONLY ErrPaymentMethodUnavailable, and only while the budget allows a
// whole further attempt. A timeout or transport failure is never retried:
// unlike the 400, a request abandoned mid-flight may already have created an
// invoice server-side, and retrying would mint a second one nobody asked for.
func (c *Client) CreateInvoice(ctx context.Context, amountSats int64, metadata map[string]interface{}) (*Invoice, error) {
	req := InvoiceRequest{
		Amount:   amountSats,
		Currency: "SATS",
		Metadata: metadata,
		Checkout: &InvoiceCheckout{
			SpeedPolicy:       "HighSpeed",                      // Immediate confirmation for Lightning
			PaymentMethods:    []string{"BTC-LightningNetwork"}, // Lightning only
			ExpirationMinutes: 60,                               // 1 hour
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()

	url := fmt.Sprintf("%s/api/v1/stores/%s/invoices", c.baseURL, c.storeID)

	for attempt := 1; ; attempt++ {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, c.attemptTimeout)
		status, respBody, err := c.send(attemptCtx, "POST", url, body)
		attemptCancel()

		if err != nil {
			return nil, fmt.Errorf("btcpay create invoice: %w", err)
		}

		if status == http.StatusOK || status == http.StatusCreated {
			var invoice Invoice
			if err := json.Unmarshal(respBody, &invoice); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			if attempt > 1 {
				slog.Info("btcpay invoice created after retry",
					"attempts", attempt, "invoice_id", invoice.ID)
			}
			return &invoice, nil
		}

		if !isTransientLNFailure(status, string(respBody)) {
			return nil, fmt.Errorf("BTCPay returned status %d: %s", status, string(respBody))
		}

		lastErr := fmt.Errorf("%w (attempt %d): %s", ErrPaymentMethodUnavailable, attempt, string(respBody))
		slog.Warn("btcpay lightning unavailable, retrying if budget allows",
			"attempt", attempt, "status", status)

		if attempt >= maxInvoiceAttempts {
			return nil, lastErr
		}

		// Only start another attempt if a whole one still fits. Beginning one we
		// cannot finish burns the remaining budget and still fails.
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < c.attemptTimeout+c.retryWait {
			return nil, lastErr
		}
		select {
		case <-time.After(c.retryWait):
		case <-ctx.Done():
			return nil, lastErr
		}
	}
}

// GetInvoice retrieves an invoice by ID
func (c *Client) GetInvoice(ctx context.Context, invoiceID string) (*Invoice, error) {
	ctx, cancel := context.WithTimeout(ctx, c.attemptTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/api/v1/stores/%s/invoices/%s", c.baseURL, c.storeID, invoiceID)
	status, respBody, err := c.send(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("BTCPay returned status %d: %s", status, string(respBody))
	}

	var invoice Invoice
	if err := json.Unmarshal(respBody, &invoice); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return &invoice, nil
}

// GetInvoicePaymentMethods retrieves payment methods for an invoice (includes BOLT11)
func (c *Client) GetInvoicePaymentMethods(ctx context.Context, invoiceID string) ([]InvoicePaymentMethod, error) {
	ctx, cancel := context.WithTimeout(ctx, c.attemptTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/api/v1/stores/%s/invoices/%s/payment-methods", c.baseURL, c.storeID, invoiceID)
	status, respBody, err := c.send(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("BTCPay returned status %d: %s", status, string(respBody))
	}

	var methods []InvoicePaymentMethod
	if err := json.Unmarshal(respBody, &methods); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return methods, nil
}

// VerifyWebhookSignature verifies the HMAC-SHA256 signature of a webhook
func (c *Client) VerifyWebhookSignature(body []byte, signature string) bool {
	if c.webhookSecret == "" {
		return false
	}

	// BTCPay sends signature as "sha256=<hex>"
	signature = strings.TrimPrefix(signature, "sha256=")

	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write(body)
	expectedMAC := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expectedMAC), []byte(signature))
}

// ParseWebhookEvent parses a webhook event from the request body
func (c *Client) ParseWebhookEvent(body []byte) (*WebhookEvent, error) {
	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("failed to unmarshal webhook: %w", err)
	}
	return &event, nil
}
