package btcpay

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"git.coldforge.xyz/coldforge/cloistr-me/internal/config"
)

// Client handles BTCPay Server API interactions
type Client struct {
	baseURL       string
	apiKey        string
	storeID       string
	webhookSecret string
	httpClient    *http.Client
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
	PaymentMethod    string `json:"paymentMethod"`
	Destination      string `json:"destination"` // BOLT11 invoice for Lightning
	PaymentLink      string `json:"paymentLink"`
	Rate             string `json:"rate"`
	PaymentMethodPaid string `json:"paymentMethodPaid"`
	TotalPaid        string `json:"totalPaid"`
	Due              string `json:"due"`
	Amount           string `json:"amount"`
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
			Timeout: 30 * time.Second,
		},
	}
}

// IsConfigured returns true if BTCPay is configured
func (c *Client) IsConfigured() bool {
	return c.baseURL != "" && c.apiKey != "" && c.storeID != ""
}

// CreateInvoice creates a new invoice for the specified amount in satoshis
func (c *Client) CreateInvoice(amountSats int64, metadata map[string]interface{}) (*Invoice, error) {
	req := InvoiceRequest{
		Amount:   amountSats,
		Currency: "SATS",
		Metadata: metadata,
		Checkout: &InvoiceCheckout{
			SpeedPolicy:       "HighSpeed",                  // Immediate confirmation for Lightning
			PaymentMethods:    []string{"BTC-LightningNetwork"}, // Lightning only
			ExpirationMinutes: 60,                           // 1 hour
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/stores/%s/invoices", c.baseURL, c.storeID)
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "token "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("BTCPay returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var invoice Invoice
	if err := json.Unmarshal(respBody, &invoice); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &invoice, nil
}

// GetInvoice retrieves an invoice by ID
func (c *Client) GetInvoice(invoiceID string) (*Invoice, error) {
	url := fmt.Sprintf("%s/api/v1/stores/%s/invoices/%s", c.baseURL, c.storeID, invoiceID)
	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Authorization", "token "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("BTCPay returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var invoice Invoice
	if err := json.Unmarshal(respBody, &invoice); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &invoice, nil
}

// GetInvoicePaymentMethods retrieves payment methods for an invoice (includes BOLT11)
func (c *Client) GetInvoicePaymentMethods(invoiceID string) ([]InvoicePaymentMethod, error) {
	url := fmt.Sprintf("%s/api/v1/stores/%s/invoices/%s/payment-methods", c.baseURL, c.storeID, invoiceID)
	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Authorization", "token "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("BTCPay returned status %d: %s", resp.StatusCode, string(respBody))
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
