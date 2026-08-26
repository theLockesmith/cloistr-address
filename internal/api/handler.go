package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/btcpay"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/crypto"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/metrics"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// Handler handles HTTP API requests
type Handler struct {
	cfg          *config.Config
	store        *storage.Storage
	btcpay       *btcpay.Client
	nwcEncryptor *crypto.Encryptor
}

// NewHandler creates a new API handler
func NewHandler(cfg *config.Config, store *storage.Storage) *Handler {
	h := &Handler{
		cfg:    cfg,
		store:  store,
		btcpay: btcpay.NewClient(cfg.BTCPay),
	}

	// Initialize NWC encryptor if key is configured
	if cfg.NWC.EncryptionKey != "" {
		enc, err := crypto.NewEncryptor(cfg.NWC.EncryptionKey)
		if err != nil {
			slog.Warn("failed to initialize NWC encryptor, NWC mode will be unavailable",
				"error", err)
		} else {
			h.nwcEncryptor = enc
		}
	}

	return h
}

// Router creates and configures the Gin router
func (h *Handler) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	// Middleware
	r.Use(gin.Recovery())
	r.Use(h.loggingMiddleware())
	r.Use(h.metricsMiddleware())
	r.Use(h.corsMiddleware())

	// Health check
	r.GET("/health", h.healthCheck)

	// Prometheus metrics
	r.GET("/metrics", gin.WrapH(metrics.Handler()))

	// NIP-05 endpoint
	r.GET("/.well-known/nostr.json", h.handleNIP05)

	// Lightning Address (LNURLP) endpoints
	r.GET("/.well-known/lnurlp/:username", h.handleLNURLPConfig)
	r.GET("/.well-known/lnurlp/:username/callback", h.handleLNURLPCallback)

	// One validator, shared by the required-auth middleware below and the
	// OPTIONAL one on the availability check.
	nip98Validator := auth.NewNIP98Validator(auth.DefaultNIP98Config())

	// Public API endpoints
	api := r.Group("/api/v1")
	{
		// Address availability check (public, but auth-AWARE).
		//
		// It stays open to anonymous callers — the signup page needs it before
		// anyone has signed in. But when the caller DOES present a valid NIP-98
		// header we price for them, because "one free name per account" cannot
		// be answered without knowing the account: a signed-in user who already
		// has a name was being told a second one was Free and then charged at
		// the quote.
		api.GET("/addresses/check/:username",
			h.optionalNIP98Middleware(nip98Validator), h.checkUsernameAvailability)

		// BTCPay webhook (no auth - signature verified in handler)
		api.POST("/webhook/payment", h.handleBTCPayWebhook)
	}

	// Authenticated API endpoints (require NIP-98)
	authAPI := r.Group("/api/v1")
	authAPI.Use(h.nip98AuthMiddleware(nip98Validator))
	{
		// Address management
		authAPI.GET("/addresses/me", h.getMyAddress)
		authAPI.PUT("/addresses/lightning", h.updateLightningConfig)

		// Purchase flow (race-based: first payment wins)
		authAPI.POST("/purchase/quote", h.getPurchaseQuote)
		authAPI.POST("/purchase/invoice", h.createPurchaseInvoice)

		// Credits (withdrawable balance from race losses)
		authAPI.GET("/credits", h.getCredits)
		authAPI.POST("/credits/withdraw", h.withdrawCredits)

		// Transfer
		authAPI.POST("/addresses/transfer", h.transferAddress)
	}

	// Internal API endpoints (for service-to-service calls)
	internalAPI := r.Group("/internal/v1")
	internalAPI.Use(h.internalAuthMiddleware())
	{
		// Credit management (used by cloistr-relay for relay bundle credits)
		internalAPI.POST("/credits/grant", h.grantCredits)

		// Address verification (used by cloistr-email to verify ownership)
		internalAPI.GET("/addresses/verify", h.verifyAddress)

		// Quota check + usage recording (for services without a direct DB connection)
		internalAPI.GET("/quotas/check", h.checkQuota)
		internalAPI.POST("/quotas/usage", h.recordQuotaUsage)
	}

	// Admin interface (NIP-98 signed + platform-admin authorized). See admin.go.
	h.registerAdminRoutes(r)

	return r
}

// nip98AuthMiddleware authenticates the request via NIP-98 and auto-provisions the
// caller's platform identity. Extension/NIP-07 users never go through a registration
// flow, so without this they'd have no users row — and has_service_access() now checks
// users.enabled before the free-tier shortcut, which would lock them out of everything.
// EnsureUser runs synchronously (before downstream handlers, so access checks see the
// row); the readable auto-assigned address is best-effort and runs in the background so
// it never adds latency to or fails the request.
func (h *Handler) nip98AuthMiddleware(validator *auth.NIP98Validator) gin.HandlerFunc {
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

		c.Set(auth.PubkeyContextKey, pubkey)

		if err := h.store.EnsureUser(c.Request.Context(), pubkey); err != nil {
			// Non-fatal: log and continue. A transient failure here shouldn't 500 the
			// whole request; the row will be created on a later request.
			slog.Warn("auto-provision users row failed", "pubkey", pubkey, "error", err)
		}

		// A user who is CLAIMING a name is not a nameless identity, so do not hand
		// them a throwaway one on the way past.
		//
		// The signup flow is: unauthenticated availability check, then an
		// authenticated POST /purchase/quote, then POST /purchase/invoice. That
		// middle call used to provision an adjective-noun-NNNN address seconds
		// before the real claim, and AtomicRegisterAddress would then promote the
		// claimed name and KEEP the auto one as a permanent alias. Every user
		// registering through the purchase screen would end up with a spare
		// address they never asked for, and a name burned out of the namespace.
		//
		// Nobody has hit it in production yet (auto-provisioning landed
		// 2026-07-20, after the only claimed name), so this is closed before it
		// can produce its first instance rather than after.
		//
		// The address still gets provisioned on every other authenticated path,
		// which is what keeps mail delivery and zaps working for an identity that
		// never claims a name.
		if !isNameClaimPath(c.Request.URL.Path) {
			go func(pk string) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if _, err := h.store.EnsureAutoAddress(ctx, pk, h.cfg.Domain); err != nil {
					slog.Debug("ensure auto address failed", "pubkey", pk, "error", err)
				}
			}(pubkey)
		}

		c.Next()
	}
}

// isNameClaimPath reports whether a request is part of claiming a name.
//
// Deliberately a suffix match on the two purchase endpoints rather than a
// prefix on /purchase: a future /purchase/... route that is NOT a claim should
// have to opt in here, instead of silently inheriting the skip.
func isNameClaimPath(path string) bool {
	return strings.HasSuffix(path, "/purchase/quote") || strings.HasSuffix(path, "/purchase/invoice")
}

// optionalNIP98Middleware identifies the caller when it can, and never rejects.
//
// Unlike nip98AuthMiddleware it does not 401: an absent or invalid header simply
// leaves the pubkey unset and the handler answers anonymously. It also does NOT
// auto-provision a user or an auto address — a price check is not a sign-up, and
// creating rows for anyone who types a name into the availability box would hand
// out an address per curious visitor.
func (h *Handler) optionalNIP98Middleware(validator *auth.NIP98Validator) gin.HandlerFunc {
	return func(c *gin.Context) {
		if pubkey, err := validator.ValidateRequest(c.Request); err == nil {
			c.Set(auth.PubkeyContextKey, pubkey)
		}
		c.Next()
	}
}

// loggingMiddleware logs HTTP requests
func (h *Handler) loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		slog.Info("http request",
			"method", c.Request.Method,
			"path", path,
			"status", status,
			"latency_ms", latency.Milliseconds(),
			"client_ip", c.ClientIP(),
		)
	}
}

// metricsMiddleware records HTTP metrics
func (h *Handler) metricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}

		c.Next()

		latency := time.Since(start).Seconds()
		status := strconv.Itoa(c.Writer.Status())

		metrics.HTTPRequestDuration.WithLabelValues(
			c.Request.Method,
			path,
			status,
		).Observe(latency)
	}
}

// corsMiddleware adds CORS headers for browser compatibility
func (h *Handler) corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// healthCheck handles health check requests
func (h *Handler) healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "cloistr-me",
		"domain":  h.cfg.Domain,
	})
}
