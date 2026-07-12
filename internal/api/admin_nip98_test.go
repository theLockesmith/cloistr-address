package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
)

// signLikeCLI mirrors cmd/cloistr-admin's signNIP98 so this test proves the CLI
// signer and the server middleware agree on the NIP-98 + payload-binding contract.
func signLikeCLI(t *testing.T, sk, method, fullURL string, body []byte) string {
	t.Helper()
	tags := nostr.Tags{nostr.Tag{"u", fullURL}, nostr.Tag{"method", method}}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		tags = append(tags, nostr.Tag{"payload", hex.EncodeToString(sum[:])})
	}
	ev := nostr.Event{CreatedAt: nostr.Now(), Kind: 27235, Tags: tags}
	if err := ev.Sign(sk); err != nil {
		t.Fatalf("sign: %v", err)
	}
	raw, _ := json.Marshal(ev)
	return "Nostr " + base64.StdEncoding.EncodeToString(raw)
}

func TestNIP98SignVerifyRoundTrip(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatal(err)
	}

	const fullURL = "http://example.com/admin/v1/addresses/grant"
	body := []byte(`{"pubkey":"` + strings.Repeat("a", 64) + `","username":"chuck"}`)
	header := signLikeCLI(t, sk, "POST", fullURL, body)

	req := httptest.NewRequest("POST", fullURL, strings.NewReader(string(body)))
	req.Header.Set("Authorization", header)

	// Server-side: validator recovers the pubkey.
	validator := auth.NewNIP98Validator(auth.DefaultNIP98Config())
	got, err := validator.ValidateRequest(req)
	if err != nil {
		t.Fatalf("ValidateRequest: %v", err)
	}
	if got != pk {
		t.Fatalf("pubkey mismatch: got %s want %s", got, pk)
	}

	// Server-side: payload tag binds the body.
	ev, err := parseNIP98Event(req)
	if err != nil {
		t.Fatalf("parseNIP98Event: %v", err)
	}
	want := getTag(ev.Tags, "payload")
	sum := sha256.Sum256(body)
	if !strings.EqualFold(want, hex.EncodeToString(sum[:])) {
		t.Fatalf("payload tag %q != body hash", want)
	}
}

func TestNIP98BodyTamperDetected(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	const fullURL = "http://example.com/admin/v1/addresses/grant"
	orig := []byte(`{"username":"chuck"}`)
	header := signLikeCLI(t, sk, "POST", fullURL, orig)

	// Attacker swaps the body but keeps the signed header.
	tampered := []byte(`{"username":"attacker"}`)
	req := httptest.NewRequest("POST", fullURL, strings.NewReader(string(tampered)))
	req.Header.Set("Authorization", header)

	ev, err := parseNIP98Event(req)
	if err != nil {
		t.Fatal(err)
	}
	want := getTag(ev.Tags, "payload")
	sum := sha256.Sum256(tampered)
	if strings.EqualFold(want, hex.EncodeToString(sum[:])) {
		t.Fatal("tampered body should NOT match signed payload hash")
	}
}
