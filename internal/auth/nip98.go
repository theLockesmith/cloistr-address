package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nbd-wtf/go-nostr"
)

const (
	// NIP98Kind is the kind for NIP-98 HTTP Auth events
	NIP98Kind = 27235

	// DefaultMaxTimeDrift is the maximum allowed timestamp drift (60 seconds per NIP-98 spec)
	DefaultMaxTimeDrift = 60 * time.Second

	// PubkeyContextKey is the context key for the authenticated pubkey
	PubkeyContextKey = "auth_pubkey"
)

var (
	ErrMissingAuth   = errors.New("missing Authorization header")
	ErrInvalidFormat = errors.New("invalid Authorization format, expected 'Nostr <base64>'")
	ErrInvalidBase64 = errors.New("invalid base64 encoding")
	ErrInvalidEvent  = errors.New("invalid event JSON")
	ErrWrongKind     = errors.New("invalid event kind, expected 27235")
	ErrInvalidSig    = errors.New("invalid signature")
	ErrExpiredEvent  = errors.New("event timestamp too old or in future")
	ErrMissingURL    = errors.New("missing 'u' (URL) tag")
	ErrURLMismatch   = errors.New("URL mismatch")
	ErrMissingMethod = errors.New("missing 'method' tag")
	ErrMethodMismatch = errors.New("method mismatch")
)

// NIP98Config configures NIP-98 validation
type NIP98Config struct {
	MaxTimeDrift       time.Duration
	RequirePayloadHash bool
}

// DefaultNIP98Config returns sensible defaults
func DefaultNIP98Config() NIP98Config {
	return NIP98Config{
		MaxTimeDrift:       DefaultMaxTimeDrift,
		RequirePayloadHash: false,
	}
}

// NIP98Validator validates NIP-98 HTTP Auth events
type NIP98Validator struct {
	config NIP98Config
}

// NewNIP98Validator creates a new validator
func NewNIP98Validator(config NIP98Config) *NIP98Validator {
	if config.MaxTimeDrift == 0 {
		config.MaxTimeDrift = DefaultMaxTimeDrift
	}
	return &NIP98Validator{config: config}
}

// ValidateRequest validates a NIP-98 Authorization header
// Returns the authenticated pubkey on success
func (v *NIP98Validator) ValidateRequest(r *http.Request) (string, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", ErrMissingAuth
	}

	if !strings.HasPrefix(authHeader, "Nostr ") {
		return "", ErrInvalidFormat
	}

	// Decode base64 event
	base64Event := strings.TrimPrefix(authHeader, "Nostr ")
	eventJSON, err := base64.StdEncoding.DecodeString(base64Event)
	if err != nil {
		return "", ErrInvalidBase64
	}

	// Parse event
	var event nostr.Event
	if err := json.Unmarshal(eventJSON, &event); err != nil {
		return "", ErrInvalidEvent
	}

	// Validate event
	if err := v.validateEvent(&event, r); err != nil {
		return "", err
	}

	return event.PubKey, nil
}

// validateEvent validates all NIP-98 requirements
func (v *NIP98Validator) validateEvent(event *nostr.Event, r *http.Request) error {
	// Check kind (must be 27235)
	if event.Kind != NIP98Kind {
		return ErrWrongKind
	}

	// Verify signature
	ok, err := event.CheckSignature()
	if err != nil || !ok {
		return ErrInvalidSig
	}

	// Check timestamp (must be within drift window)
	eventTime := event.CreatedAt.Time()
	drift := time.Since(eventTime)
	if drift < 0 {
		drift = -drift
	}
	if drift > v.config.MaxTimeDrift {
		return ErrExpiredEvent
	}

	// Validate URL tag
	urlTag := getTagValue(event.Tags, "u")
	if urlTag == "" {
		return ErrMissingURL
	}

	expectedURL := buildRequestURL(r)
	if !urlMatches(urlTag, expectedURL) {
		slog.Debug("URL mismatch", "expected", expectedURL, "got", urlTag)
		return ErrURLMismatch
	}

	// Validate method tag
	methodTag := getTagValue(event.Tags, "method")
	if methodTag == "" {
		return ErrMissingMethod
	}
	if !strings.EqualFold(methodTag, r.Method) {
		return ErrMethodMismatch
	}

	return nil
}

// getTagValue retrieves the first value for a tag
func getTagValue(tags nostr.Tags, name string) string {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name {
			return tag[1]
		}
	}
	return ""
}

// buildRequestURL reconstructs the full request URL for validation
func buildRequestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Check for forwarded protocol (behind reverse proxy/tunnel)
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}

	host := r.Host
	if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
		host = fwdHost
	}

	// Include query string in URL matching
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path = path + "?" + r.URL.RawQuery
	}

	return scheme + "://" + host + path
}

// urlMatches compares URLs allowing for minor variations
func urlMatches(a, b string) bool {
	// Normalize trailing slashes
	a = strings.TrimSuffix(a, "/")
	b = strings.TrimSuffix(b, "/")

	// Direct match
	if a == b {
		return true
	}

	// Case-insensitive comparison (hosts can have different cases)
	return strings.EqualFold(a, b)
}

// NIP98Middleware creates Gin middleware for NIP-98 authentication
func NIP98Middleware(validator *NIP98Validator) gin.HandlerFunc {
	return func(c *gin.Context) {
		pubkey, err := validator.ValidateRequest(c.Request)
		if err != nil {
			slog.Debug("NIP-98 auth failed", "error", err, "path", c.Request.URL.Path)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":   "Authentication required",
				"details": err.Error(),
			})
			return
		}

		// Store pubkey in context for handlers
		c.Set(PubkeyContextKey, pubkey)
		c.Next()
	}
}

// GetPubkey retrieves the authenticated pubkey from Gin context
func GetPubkey(c *gin.Context) string {
	if pubkey, exists := c.Get(PubkeyContextKey); exists {
		if pk, ok := pubkey.(string); ok {
			return pk
		}
	}
	return ""
}
