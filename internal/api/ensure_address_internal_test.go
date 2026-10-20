package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func ensureCall(t *testing.T, r http.Handler, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://example.com/internal/v1/addresses/ensure", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestInternalEnsureAddress(t *testing.T) {
	r, db := ensureTestRouter(t)
	pk, _ := nostr.GetPublicKey(nostr.GeneratePrivateKey())
	cleanupEnsurePubkey(t, db, pk)
	body := `{"pubkey":"` + pk + `"}`

	w := ensureCall(t, r, testInternalSecret, body)
	if w.Code != http.StatusOK {
		t.Fatalf("ensure = %d %s; want 200", w.Code, w.Body.String())
	}
	var first EnsureAddressResponse
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first.Username == "" || first.Domain != "cloistr.xyz" || first.Pubkey != pk || !first.Verified || !first.Created {
		t.Fatalf("first ensure = %+v; want a new verified address", first)
	}

	// Idempotent: the same address comes back, nothing new is created.
	w = ensureCall(t, r, testInternalSecret, body)
	var second EnsureAddressResponse
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	if w.Code != http.StatusOK || second.Username != first.Username || second.Created {
		t.Fatalf("second ensure = %d %+v; want 200, same username %q, created=false", w.Code, second, first.Username)
	}
	if n := activeAddressCount(t, db, pk); n != 1 {
		t.Fatalf("pubkey has %d active addresses after two ensures; want 1", n)
	}
}

func TestInternalEnsureAddress_Refusals(t *testing.T) {
	r, _ := ensureTestRouter(t)
	pk := strings.Repeat("ab", 32)
	if w := ensureCall(t, r, "", `{"pubkey":"`+pk+`"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("no secret = %d; want 401", w.Code)
	}
	if w := ensureCall(t, r, "wrong", `{"pubkey":"`+pk+`"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong secret = %d; want 401", w.Code)
	}
	for _, bad := range []string{`{}`, `{"pubkey":"abc"}`, `{"pubkey":"` + strings.Repeat("zz", 32) + `"}`, `{"pubkey":"` + strings.ToUpper(pk) + `"}`} {
		if w := ensureCall(t, r, testInternalSecret, bad); w.Code != http.StatusBadRequest {
			t.Errorf("body %s = %d; want 400", bad, w.Code)
		}
	}
}
