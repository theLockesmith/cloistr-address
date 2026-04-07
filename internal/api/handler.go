package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"git.coldforge.xyz/coldforge/cloistr-address/internal/auth"
	"git.coldforge.xyz/coldforge/cloistr-address/internal/config"
	"git.coldforge.xyz/coldforge/cloistr-address/internal/metrics"
	"git.coldforge.xyz/coldforge/cloistr-address/internal/storage"
)

// Handler handles HTTP API requests
type Handler struct {
	cfg   *config.Config
	store *storage.Storage
}

// NewHandler creates a new API handler
func NewHandler(cfg *config.Config, store *storage.Storage) *Handler {
	return &Handler{
		cfg:   cfg,
		store: store,
	}
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

	// Public API endpoints
	api := r.Group("/api/v1")
	{
		// Address availability check (public)
		api.GET("/addresses/check/:username", h.checkUsernameAvailability)
	}

	// Authenticated API endpoints (require NIP-98)
	nip98Validator := auth.NewNIP98Validator(auth.DefaultNIP98Config())
	authAPI := r.Group("/api/v1")
	authAPI.Use(auth.NIP98Middleware(nip98Validator))
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

	return r
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
		"service": "cloistr-address",
		"domain":  h.cfg.Domain,
	})
}
