package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// Served-domain administration, proxied to cloistr-email.
//
// Topology (architecture/email-domain-management-decisions.md §2). The admin page
// is a browser SPA and cannot hold cloistr-email's internal secret — it would ship
// in the bundle and expose that API to a browser origin. So there are two hops:
//
//	browser (cloistr-admin-ui) --NIP-98, platform-admin--> cloistr-me /admin/v1/domains/*
//	cloistr-me --Bearer <cloistr-email secret>--> cloistr-email /internal/v1/domains/*
//
// cloistr-me stays a thin authenticated proxy. It holds no DKIM key material and
// no email-schema grant: key generation, storage, and the signer-map reload all
// remain inside cloistr-email, which is the only process that signs.

// maxDomainResponseBytes caps what we will read back from cloistr-email. The
// payloads are small (a domain list with DNS records); the cap keeps a wedged or
// hostile upstream from ballooning an admin request.
const maxDomainResponseBytes = 1 << 20 // 1 MiB

// domainNameRe constrains the {domain} path segment before it is interpolated
// into the upstream URL. Rejecting anything with a slash, dot-segment, or
// non-hostname character means a crafted path cannot escape /internal/v1/domains/
// and reach another endpoint on the internal API.
var domainNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// emailInternalEnabled reports whether both halves of the upstream config are
// present. Without them the routes are never registered.
func (h *Handler) emailInternalEnabled() bool {
	return h.cfg.EmailInternal.URL != "" && h.cfg.EmailInternal.Secret != ""
}

// registerAdminDomainRoutes wires /admin/v1/domains/* onto an already
// NIP-98 + platform-admin gated group. Authz is admin-only by design: DKIM key
// handling, DNS verification, and served-domain activation are platform
// operations with real deliverability blast radius — a bad active domain can
// poison shared outbound reputation. Self-serve BYO-domain is a separate,
// more constrained surface (ownership proof, rate limits, abuse review).
func (h *Handler) registerAdminDomainRoutes(admin *gin.RouterGroup) {
	if !h.emailInternalEnabled() {
		slog.Warn("EMAIL_INTERNAL_URL/EMAIL_INTERNAL_SECRET not set — /admin/v1/domains proxy disabled")
		return
	}
	admin.GET("/domains", h.adminListDomains)
	admin.POST("/domains", h.adminCreateDomain)
	admin.DELETE("/domains/:domain", h.adminDeleteDomain)
	admin.POST("/domains/:domain/verify", h.adminVerifyDomain)
	admin.POST("/domains/:domain/activate", h.adminActivateDomain)
	admin.POST("/domains/:domain/deactivate", h.adminDeactivateDomain)
	admin.POST("/domains/:domain/rotate-dkim", h.adminRotateDKIM)
	slog.Info("admin domain proxy enabled", "upstream", h.cfg.EmailInternal.URL)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) adminListDomains(c *gin.Context) {
	h.proxyToEmail(c, http.MethodGet, "", nil, "")
}

// adminCreateDomain registers a domain and returns the DNS records to publish.
//
// dkim_private_key is deliberately NOT forwarded even though cloistr-email
// accepts it for out-of-band key import. Passing it here would route a DKIM
// private key through the browser and through this service, which is exactly
// the exposure this topology exists to avoid. Import stays an operator task
// performed directly against cloistr-email; through the admin page, keys are
// always generated upstream.
func (h *Handler) adminCreateDomain(c *gin.Context) {
	var req struct {
		Domain   string `json:"domain" binding:"required"`
		Selector string `json:"selector,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if !domainNameRe.MatchString(domain) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid domain"})
		return
	}
	body, err := json.Marshal(map[string]string{"domain": domain, "selector": strings.TrimSpace(req.Selector)})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode request"})
		return
	}
	h.proxyToEmail(c, http.MethodPost, "", body, "domain_create:"+domain)
}

func (h *Handler) adminDeleteDomain(c *gin.Context) {
	h.domainAction(c, http.MethodDelete, "", "domain_delete")
}

func (h *Handler) adminVerifyDomain(c *gin.Context) {
	// Read-only against public DNS upstream; still audited, since it flips
	// the stored verified flag.
	h.domainAction(c, http.MethodPost, "/verify", "domain_verify")
}

func (h *Handler) adminActivateDomain(c *gin.Context) {
	h.domainAction(c, http.MethodPost, "/activate", "domain_activate")
}

func (h *Handler) adminDeactivateDomain(c *gin.Context) {
	h.domainAction(c, http.MethodPost, "/deactivate", "domain_deactivate")
}

func (h *Handler) adminRotateDKIM(c *gin.Context) {
	h.domainAction(c, http.MethodPost, "/rotate-dkim", "domain_rotate_dkim")
}

// domainAction validates the :domain segment and proxies a per-domain action.
func (h *Handler) domainAction(c *gin.Context, method, suffix, auditAction string) {
	domain := strings.ToLower(strings.TrimSpace(c.Param("domain")))
	if !domainNameRe.MatchString(domain) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid domain"})
		return
	}
	h.proxyToEmail(c, method, "/"+url.PathEscape(domain)+suffix, nil, auditAction+":"+domain)
}

// ---------------------------------------------------------------------------
// Proxy
// ---------------------------------------------------------------------------

// emailHTTPClient is the outbound client for cloistr-email's internal API.
// Bounded so a slow upstream surfaces as a 502 rather than holding an admin
// request open indefinitely.
var emailHTTPClient = &http.Client{Timeout: 20 * time.Second}

// proxyToEmail forwards to cloistr-email's /internal/v1/domains{path} with the
// Bearer secret and relays the upstream status and body verbatim.
//
// Verbatim relay is deliberate: cloistr-email's responses are already public-only
// (it never serializes dkim_private_key, even on create/rotate), so re-shaping
// them here would only add a place for the contract to drift. The Bearer secret
// exists solely on this side of the hop and is never echoed.
//
// auditAction is empty for reads; a non-empty value writes an admin audit row
// after a successful (2xx) upstream call.
func (h *Handler) proxyToEmail(c *gin.Context, method, path string, body []byte, auditAction string) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()

	endpoint := h.cfg.EmailInternal.URL + "/internal/v1/domains" + path
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		slog.Error("domain proxy: build request failed", "error", err, "endpoint", endpoint)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reach email service"})
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.EmailInternal.Secret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := emailHTTPClient.Do(req)
	if err != nil {
		slog.Error("domain proxy: upstream call failed", "error", err, "method", method, "path", path)
		c.JSON(http.StatusBadGateway, gin.H{"error": "email service unavailable"})
		return
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxDomainResponseBytes))
	if err != nil {
		slog.Error("domain proxy: reading upstream body failed", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "email service returned an unreadable response"})
		return
	}

	if auditAction != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.auditDomainAction(c, auditAction, resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	c.Data(resp.StatusCode, ct, payload)
}

// auditDomainAction records the mutation in the hash-chained admin audit log.
// A failure to audit is logged but does not fail the request — the upstream
// change has already been applied, and reporting it as an error would be worse
// than a gap in the chain.
func (h *Handler) auditDomainAction(c *gin.Context, action string, status int) {
	if h.store == nil {
		// Only reachable in tests that exercise the proxy without a database.
		// Guarded rather than assumed: this runs AFTER the upstream mutation has
		// already been applied, so anything that panics here would turn a
		// successful domain change into a 500 for the operator.
		return
	}
	actor, sig := adminActor(c)
	parts := strings.SplitN(action, ":", 2)
	verb, subject := parts[0], ""
	if len(parts) == 2 {
		subject = parts[1]
	}
	err := h.store.LogAdminAudit(c.Request.Context(), storage.AuditEntry{
		TableName:   "email_domains",
		RecordID:    subject,
		Action:      verb,
		ActorPubkey: actor,
		NewValues:   map[string]any{"domain": subject, "upstream_status": status},
		Metadata:    map[string]any{"source": "admin_domain_proxy", "user_agent": c.Request.UserAgent()},
		Signature:   sig,
	})
	if err != nil {
		slog.Error("domain proxy: audit write failed", "error", err, "action", action)
	}
}
