package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
)

// domainProxyHarness stands up a fake cloistr-email internal API plus a gin
// engine with the domain routes mounted WITHOUT the admin auth middleware, so
// each test exercises the proxy itself rather than re-testing NIP-98 (that is
// covered by TestAdminDomainRoutesRejectMissingAuth below, which goes through
// the real gate).
type domainProxyHarness struct {
	upstream *httptest.Server
	engine   *gin.Engine

	// last request seen by the fake upstream
	gotAuth   string
	gotMethod string
	gotPath   string
	gotBody   []byte

	// what the fake upstream should answer with
	respStatus int
	respBody   string
}

func newDomainProxyHarness(t *testing.T) *domainProxyHarness {
	t.Helper()
	h := &domainProxyHarness{respStatus: http.StatusOK, respBody: `{"domains":[]}`}

	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.gotAuth = r.Header.Get("Authorization")
		h.gotMethod = r.Method
		h.gotPath = r.URL.Path
		h.gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.respStatus)
		_, _ = io.WriteString(w, h.respBody)
	}))
	t.Cleanup(h.upstream.Close)

	gin.SetMode(gin.TestMode)
	h.engine = gin.New()
	handler := &Handler{cfg: &config.Config{
		Domain: "cloistr.xyz",
		EmailInternal: config.EmailInternalConfig{
			URL:    h.upstream.URL,
			Secret: "upstream-secret",
		},
	}}
	// Mount without the auth middleware; audit writes need a store, so only
	// read paths and rejected paths are exercised here.
	handler.registerAdminDomainRoutes(h.engine.Group("/admin/v1"))
	return h
}

func (h *domainProxyHarness) do(method, path, body string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://me.cloistr.xyz"+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.engine.ServeHTTP(w, req)
	return w
}

func TestDomainProxy_ForwardsBearerAndRelaysVerbatim(t *testing.T) {
	h := newDomainProxyHarness(t)
	h.respStatus = http.StatusOK
	h.respBody = `{"domains":[{"domain":"cloistr.xyz","dkim_selector":"mail","verified":true,"active":true}]}`

	w := h.do("GET", "/admin/v1/domains", "")

	if h.gotAuth != "Bearer upstream-secret" {
		t.Errorf("upstream Authorization = %q, want %q", h.gotAuth, "Bearer upstream-secret")
	}
	if h.gotPath != "/internal/v1/domains" {
		t.Errorf("upstream path = %q, want %q", h.gotPath, "/internal/v1/domains")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != h.respBody {
		t.Errorf("body was not relayed verbatim:\n got %s\nwant %s", got, h.respBody)
	}
	if strings.Contains(w.Body.String(), "upstream-secret") {
		t.Error("the internal secret leaked into the client-facing response")
	}
}

func TestDomainProxy_RelaysUpstreamErrorStatus(t *testing.T) {
	h := newDomainProxyHarness(t)
	h.respStatus = http.StatusConflict
	h.respBody = `{"error":{"code":"DOMAIN_EXISTS"}}`

	w := h.do("POST", "/admin/v1/domains", `{"domain":"example.com"}`)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (upstream status must pass through)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "DOMAIN_EXISTS") {
		t.Errorf("upstream error body was not relayed: %s", w.Body.String())
	}
}

// The browser must never be able to push a DKIM private key through this hop —
// that is the whole reason cloistr-me proxies instead of the SPA calling the
// internal API directly.
func TestDomainProxy_StripsDKIMPrivateKeyFromCreate(t *testing.T) {
	h := newDomainProxyHarness(t)
	h.respStatus = http.StatusCreated
	h.respBody = `{"domain":"example.com"}`

	w := h.do("POST", "/admin/v1/domains",
		`{"domain":"example.com","selector":"mail","dkim_private_key":"-----BEGIN RSA PRIVATE KEY-----"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if strings.Contains(string(h.gotBody), "dkim_private_key") ||
		strings.Contains(string(h.gotBody), "BEGIN RSA PRIVATE KEY") {
		t.Errorf("dkim_private_key was forwarded upstream: %s", h.gotBody)
	}
	var sent map[string]string
	if err := json.Unmarshal(h.gotBody, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if sent["domain"] != "example.com" || sent["selector"] != "mail" {
		t.Errorf("domain/selector not forwarded correctly: %v", sent)
	}
}

func TestDomainProxy_NormalizesAndValidatesDomain(t *testing.T) {
	h := newDomainProxyHarness(t)

	// Uppercase and surrounding space are normalized rather than rejected.
	h.respStatus = http.StatusCreated
	h.respBody = `{}`
	if w := h.do("POST", "/admin/v1/domains", `{"domain":"  Example.COM  "}`); w.Code != http.StatusCreated {
		t.Fatalf("normalized domain: status = %d, want 201", w.Code)
	}
	var sent map[string]string
	_ = json.Unmarshal(h.gotBody, &sent)
	if sent["domain"] != "example.com" {
		t.Errorf("domain not normalized: got %q, want %q", sent["domain"], "example.com")
	}

	for _, bad := range []string{"", "nodot", "-leading.com", "trailing-.com", "sp ace.com", "under_score.com"} {
		body, _ := json.Marshal(map[string]string{"domain": bad})
		w := h.do("POST", "/admin/v1/domains", string(body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("domain %q: status = %d, want 400", bad, w.Code)
		}
	}
}

// A crafted :domain segment must not be able to walk out of
// /internal/v1/domains/ and hit another endpoint on the internal API.
func TestDomainProxy_RejectsPathTraversalInDomain(t *testing.T) {
	h := newDomainProxyHarness(t)

	for _, path := range []string{
		"/admin/v1/domains/..%2f..%2fadmin/verify",
		"/admin/v1/domains/example.com%2f..%2fother/verify",
		"/admin/v1/domains/.../verify",
	} {
		h.gotPath = ""
		w := h.do("POST", path, "")
		if w.Code == http.StatusOK || w.Code == http.StatusCreated {
			t.Errorf("%s: got %d, want a rejection", path, w.Code)
		}
		if strings.HasPrefix(h.gotPath, "/internal/v1/domains/") &&
			!strings.HasPrefix(h.gotPath, "/internal/v1/domains/example.com/") {
			t.Errorf("%s reached upstream at %q", path, h.gotPath)
		}
	}
}

func TestDomainProxy_PerDomainActionsMapToUpstreamPaths(t *testing.T) {
	h := newDomainProxyHarness(t)
	h.respBody = `{}`

	cases := []struct {
		method, path, wantPath, wantMethod string
	}{
		{"POST", "/admin/v1/domains/example.com/verify", "/internal/v1/domains/example.com/verify", "POST"},
		{"POST", "/admin/v1/domains/example.com/activate", "/internal/v1/domains/example.com/activate", "POST"},
		{"POST", "/admin/v1/domains/example.com/deactivate", "/internal/v1/domains/example.com/deactivate", "POST"},
		{"POST", "/admin/v1/domains/example.com/rotate-dkim", "/internal/v1/domains/example.com/rotate-dkim", "POST"},
		{"DELETE", "/admin/v1/domains/example.com", "/internal/v1/domains/example.com", "DELETE"},
	}
	for _, c := range cases {
		h.gotPath, h.gotMethod = "", ""
		h.respStatus = http.StatusOK
		h.do(c.method, c.path, "")
		if h.gotPath != c.wantPath {
			t.Errorf("%s %s: upstream path = %q, want %q", c.method, c.path, h.gotPath, c.wantPath)
		}
		if h.gotMethod != c.wantMethod {
			t.Errorf("%s %s: upstream method = %q, want %q", c.method, c.path, h.gotMethod, c.wantMethod)
		}
	}
}

// With either half of the upstream config missing the routes must not exist at
// all — a half-configured deployment should 404, not 502 or (worse) call out
// with an empty Bearer.
func TestDomainProxy_DisabledWithoutConfig(t *testing.T) {
	for name, cfg := range map[string]config.EmailInternalConfig{
		"both missing": {},
		"url only":     {URL: "http://email.internal"},
		"secret only":  {Secret: "s"},
	} {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		h := &Handler{cfg: &config.Config{Domain: "cloistr.xyz", EmailInternal: cfg}}
		h.registerAdminDomainRoutes(r.Group("/admin/v1"))

		req := httptest.NewRequest("GET", "http://me.cloistr.xyz/admin/v1/domains", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (routes should not be registered)", name, w.Code)
		}
	}
}

// The domain routes must sit behind the same NIP-98 + platform-admin gate as
// every other /admin/v1 route.
func TestAdminDomainRoutesRejectMissingAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{cfg: &config.Config{
		Domain:        "cloistr.xyz",
		EmailInternal: config.EmailInternalConfig{URL: "http://email.internal", Secret: "s"},
	}, store: nil}
	h.registerAdminRoutes(r)

	cases := []struct{ method, path string }{
		{"GET", "/admin/v1/domains"},
		{"POST", "/admin/v1/domains"},
		{"DELETE", "/admin/v1/domains/example.com"},
		{"POST", "/admin/v1/domains/example.com/verify"},
		{"POST", "/admin/v1/domains/example.com/activate"},
		{"POST", "/admin/v1/domains/example.com/deactivate"},
		{"POST", "/admin/v1/domains/example.com/rotate-dkim"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://me.cloistr.xyz"+c.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no auth: got %d, want 401", c.method, c.path, w.Code)
		}
	}
}
