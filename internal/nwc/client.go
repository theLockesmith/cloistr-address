// Package nwc implements NIP-47 Nostr Wallet Connect client
package nwc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
	"github.com/nbd-wtf/go-nostr/nip44"
)

// NWC event kinds (NIP-47)
const (
	KindNWCRequest  = 23194
	KindNWCResponse = 23195
	KindNWCInfo     = 13194
)

// Common errors
var (
	ErrInvalidURI      = errors.New("invalid NWC connection URI")
	ErrTimeout         = errors.New("NWC request timeout")
	ErrNoResponse      = errors.New("no response from wallet")
	ErrWalletError     = errors.New("wallet returned error")
	ErrEncryption      = errors.New("encryption/decryption failed")
	ErrNotConnected    = errors.New("not connected to relay")
)

// ConnectionConfig holds parsed NWC connection details
type ConnectionConfig struct {
	WalletPubkey string
	RelayURL     string
	Secret       string // 32-byte hex secret for signing/encryption
}

// Client is a NIP-47 Nostr Wallet Connect client
type Client struct {
	config       ConnectionConfig
	clientPubkey string
	clientSecret string
	relay        *nostr.Relay
	useNIP44     bool // Whether to use NIP-44 (true) or NIP-04 (false) encryption
}

// MakeInvoiceRequest represents a make_invoice request
type MakeInvoiceRequest struct {
	Amount      int64  `json:"amount"`                 // Amount in millisatoshis
	Description string `json:"description,omitempty"`  // Invoice description
	Expiry      int64  `json:"expiry,omitempty"`       // Invoice expiry in seconds
}

// MakeInvoiceResponse represents a make_invoice response
type MakeInvoiceResponse struct {
	Invoice     string `json:"invoice"`      // BOLT-11 invoice string
	PaymentHash string `json:"payment_hash"` // Payment hash
}

// NWCRequest is the NIP-47 request payload structure
type NWCRequest struct {
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

// NWCResponse is the NIP-47 response payload structure
type NWCResponse struct {
	ResultType string          `json:"result_type"`
	Error      *NWCError       `json:"error,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

// NWCError represents an error from the wallet
type NWCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ParseConnectionURI parses a nostr+walletconnect:// URI
func ParseConnectionURI(uri string) (*ConnectionConfig, error) {
	// Format: nostr+walletconnect://<wallet_pubkey>?relay=<relay_url>&secret=<secret>
	if !strings.HasPrefix(uri, "nostr+walletconnect://") {
		return nil, fmt.Errorf("%w: must start with nostr+walletconnect://", ErrInvalidURI)
	}

	// Remove the scheme
	withoutScheme := strings.TrimPrefix(uri, "nostr+walletconnect://")

	// Split pubkey from query params
	parts := strings.SplitN(withoutScheme, "?", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: missing query parameters", ErrInvalidURI)
	}

	walletPubkey := parts[0]
	if len(walletPubkey) != 64 {
		return nil, fmt.Errorf("%w: invalid wallet pubkey length", ErrInvalidURI)
	}

	// Parse query parameters
	params, err := url.ParseQuery(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: invalid query parameters: %v", ErrInvalidURI, err)
	}

	relayURL := params.Get("relay")
	if relayURL == "" {
		return nil, fmt.Errorf("%w: missing relay parameter", ErrInvalidURI)
	}

	secret := params.Get("secret")
	if secret == "" {
		return nil, fmt.Errorf("%w: missing secret parameter", ErrInvalidURI)
	}
	if len(secret) != 64 {
		return nil, fmt.Errorf("%w: invalid secret length", ErrInvalidURI)
	}

	return &ConnectionConfig{
		WalletPubkey: walletPubkey,
		RelayURL:     relayURL,
		Secret:       secret,
	}, nil
}

// NewClient creates a new NWC client from a connection config
func NewClient(config ConnectionConfig) (*Client, error) {
	// Use the secret as the private key for the client
	pubkey, err := nostr.GetPublicKey(config.Secret)
	if err != nil {
		return nil, fmt.Errorf("invalid secret: %w", err)
	}

	return &Client{
		config:       config,
		clientPubkey: pubkey,
		clientSecret: config.Secret,
		useNIP44:     true, // Default to NIP-44, will fallback to NIP-04 if needed
	}, nil
}

// NewClientFromURI creates a new NWC client from a connection URI
func NewClientFromURI(uri string) (*Client, error) {
	config, err := ParseConnectionURI(uri)
	if err != nil {
		return nil, err
	}
	return NewClient(*config)
}

// Connect establishes a connection to the relay
func (c *Client) Connect(ctx context.Context) error {
	relay, err := nostr.RelayConnect(ctx, c.config.RelayURL)
	if err != nil {
		return fmt.Errorf("failed to connect to relay: %w", err)
	}
	c.relay = relay
	return nil
}

// Close closes the relay connection
func (c *Client) Close() {
	if c.relay != nil {
		c.relay.Close()
	}
}

// MakeInvoice requests the wallet to create a new invoice
func (c *Client) MakeInvoice(ctx context.Context, req MakeInvoiceRequest) (*MakeInvoiceResponse, error) {
	if c.relay == nil {
		return nil, ErrNotConnected
	}

	// Create the request payload
	nwcReq := NWCRequest{
		Method: "make_invoice",
		Params: req,
	}

	// Send request and wait for response
	response, err := c.sendRequest(ctx, nwcReq)
	if err != nil {
		return nil, err
	}

	// Parse the result
	var invoiceResp MakeInvoiceResponse
	if err := json.Unmarshal(response.Result, &invoiceResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &invoiceResp, nil
}

// sendRequest sends an NWC request and waits for the response
func (c *Client) sendRequest(ctx context.Context, req NWCRequest) (*NWCResponse, error) {
	// Marshal the request
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Encrypt the payload
	encrypted, err := c.encrypt(string(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncryption, err)
	}

	// Create the request event
	event := nostr.Event{
		Kind:      KindNWCRequest,
		Content:   encrypted,
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Tags: nostr.Tags{
			{"p", c.config.WalletPubkey},
		},
	}

	// Sign the event
	if err := event.Sign(c.clientSecret); err != nil {
		return nil, fmt.Errorf("failed to sign event: %w", err)
	}

	// Subscribe to responses before sending
	respChan := make(chan *NWCResponse, 1)
	errChan := make(chan error, 1)

	// Create subscription for the response
	filters := []nostr.Filter{{
		Kinds:   []int{KindNWCResponse},
		Authors: []string{c.config.WalletPubkey},
		Tags:    nostr.TagMap{"e": []string{event.ID}},
	}}

	sub, err := c.relay.Subscribe(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe: %w", err)
	}
	defer sub.Unsub()

	// Handle incoming events
	go func() {
		for ev := range sub.Events {
			resp, err := c.handleResponse(ev)
			if err != nil {
				slog.Debug("failed to handle response", "error", err)
				continue
			}
			respChan <- resp
			return
		}
	}()

	// Publish the request
	if err := c.relay.Publish(ctx, event); err != nil {
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}

	// Wait for response with timeout
	timeout := time.After(30 * time.Second)
	select {
	case resp := <-respChan:
		if resp.Error != nil {
			return nil, fmt.Errorf("%w: %s - %s", ErrWalletError, resp.Error.Code, resp.Error.Message)
		}
		return resp, nil
	case err := <-errChan:
		return nil, err
	case <-timeout:
		return nil, ErrTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// handleResponse decrypts and parses a response event
func (c *Client) handleResponse(event *nostr.Event) (*NWCResponse, error) {
	// Decrypt the content
	decrypted, err := c.decrypt(event.Content)
	if err != nil {
		// If NIP-44 fails, try NIP-04
		if c.useNIP44 {
			c.useNIP44 = false
			decrypted, err = c.decrypt(event.Content)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrEncryption, err)
			}
		} else {
			return nil, fmt.Errorf("%w: %v", ErrEncryption, err)
		}
	}

	// Parse the response
	var resp NWCResponse
	if err := json.Unmarshal([]byte(decrypted), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &resp, nil
}

// encrypt encrypts content for the wallet using NIP-44 or NIP-04
func (c *Client) encrypt(content string) (string, error) {
	if c.useNIP44 {
		// NIP-44 encryption
		conversationKey, err := nip44.GenerateConversationKey(c.config.WalletPubkey, c.clientSecret)
		if err != nil {
			return "", err
		}
		return nip44.Encrypt(content, conversationKey)
	}

	// NIP-04 encryption (fallback) - compute shared secret first
	sharedSecret, err := nip04.ComputeSharedSecret(c.config.WalletPubkey, c.clientSecret)
	if err != nil {
		return "", err
	}
	return nip04.Encrypt(content, sharedSecret)
}

// decrypt decrypts content from the wallet using NIP-44 or NIP-04
func (c *Client) decrypt(content string) (string, error) {
	if c.useNIP44 {
		// NIP-44 decryption
		conversationKey, err := nip44.GenerateConversationKey(c.config.WalletPubkey, c.clientSecret)
		if err != nil {
			return "", err
		}
		return nip44.Decrypt(content, conversationKey)
	}

	// NIP-04 decryption (fallback) - compute shared secret first
	sharedSecret, err := nip04.ComputeSharedSecret(c.config.WalletPubkey, c.clientSecret)
	if err != nil {
		return "", err
	}
	return nip04.Decrypt(content, sharedSecret)
}

// SetEncryptionMode sets whether to use NIP-44 (true) or NIP-04 (false)
func (c *Client) SetEncryptionMode(useNIP44 bool) {
	c.useNIP44 = useNIP44
}

// GetConfig returns the connection configuration
func (c *Client) GetConfig() ConnectionConfig {
	return c.config
}
