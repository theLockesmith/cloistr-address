package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nbd-wtf/go-nostr"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
)

func b64(ev nostr.Event) string {
	raw, _ := json.Marshal(ev)
	return base64.StdEncoding.EncodeToString(raw)
}

// adminRouter builds a gin engine with only the admin routes wired. store is nil
// on purpose: every case below must be rejected by adminAuthMiddleware BEFORE any
// handler (which would touch the store) runs. If a case reached the store it would
// panic on nil — so a clean status assertion also proves the middleware short-circuits.
func adminRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{cfg: &config.Config{Domain: "cloistr.xyz"}, store: nil}
	h.registerAdminRoutes(r)
	return r
}

func TestAdminRoutesRejectMissingAuth(t *testing.T) {
	r := adminRouter()
	cases := []struct{ method, path string }{
		{"GET", "/admin/v1/me"},
		{"GET", "/admin/v1/reserved"},
		{"GET", "/admin/v1/tiers"},
		{"GET", "/admin/v1/audit"},
		{"GET", "/admin/v1/users/lookup"},
		{"POST", "/admin/v1/addresses/grant"},
		{"POST", "/admin/v1/addresses/revoke"},
		{"POST", "/admin/v1/addresses/primary"},
		{"POST", "/admin/v1/addresses/nip05"},
		{"POST", "/admin/v1/reserved"},
		{"DELETE", "/admin/v1/reserved/foo"},
		{"POST", "/admin/v1/quotas"},
		{"PUT", "/admin/v1/tiers"},
		{"POST", "/admin/v1/credits/grant"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://me.cloistr.xyz"+c.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Errorf("%s %s with no auth: got %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestAdminRouteRejectsBadSignature(t *testing.T) {
	r := adminRouter()
	req := httptest.NewRequest("GET", "http://me.cloistr.xyz/admin/v1/me", nil)
	// Structurally valid header, garbage event -> signature/parse fails.
	req.Header.Set("Authorization", "Nostr "+"bm90LWEtdmFsaWQtZXZlbnQ=")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("bad signature: got %d, want 401", w.Code)
	}
}

func TestAdminMutationRejectsMissingPayloadTag(t *testing.T) {
	r := adminRouter()
	sk := nostr.GeneratePrivateKey()
	const url = "http://me.cloistr.xyz/admin/v1/addresses/grant"
	body := `{"pubkey":"` + strings.Repeat("a", 64) + `","username":"x"}`

	// Valid NIP-98 signature over method+URL, but WITHOUT a payload tag binding the body.
	ev := nostr.Event{CreatedAt: nostr.Now(), Kind: 27235, Tags: nostr.Tags{
		nostr.Tag{"u", url}, nostr.Tag{"method", "POST"},
	}}
	if err := ev.Sign(sk); err != nil {
		t.Fatal(err)
	}
	header := "Nostr " + b64(ev)

	req := httptest.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", header)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("missing payload tag on body: got %d, want 401", w.Code)
	}
}

func TestAdminMutationRejectsTamperedBody(t *testing.T) {
	r := adminRouter()
	sk := nostr.GeneratePrivateKey()
	const url = "http://me.cloistr.xyz/admin/v1/addresses/grant"
	orig := `{"username":"chuck"}`
	header := signLikeCLI(t, sk, "POST", url, []byte(orig)) // includes payload=sha256(orig)

	// Same signed header, different body.
	req := httptest.NewRequest("POST", url, strings.NewReader(`{"username":"attacker"}`))
	req.Header.Set("Authorization", header)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("tampered body: got %d, want 401", w.Code)
	}
}
