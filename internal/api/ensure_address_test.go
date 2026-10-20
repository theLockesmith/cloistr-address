package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/nbd-wtf/go-nostr"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

/*
A brand-new identity had no address until its SECOND authenticated call: the
NIP-98 middleware ran EnsureAutoAddress in a goroutine, so the dashboard's first
GET /addresses/me answered 404 "No address registered" while the address was
still being created (seen live on production, 2026-10-19). And an identity that
signed up through Mail never called cloistr-me at all, so it never got an
address. /addresses/me now ensures synchronously, and cloistr-email can ask for
the address directly through POST /internal/v1/addresses/ensure.
*/

const testInternalSecret = "ensure-test-secret"

func ensureTestRouter(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB-integration test")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := &config.Config{Domain: "cloistr.xyz"}
	cfg.InternalAPI.Secret = testInternalSecret
	return NewHandler(cfg, storage.NewWithDB(db)).Router(), db
}

func cleanupEnsurePubkey(t *testing.T, db *sql.DB, pubkey string) {
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM address_ownership WHERE pubkey = $1`,
			`DELETE FROM addresses WHERE pubkey = $1`,
			`DELETE FROM users WHERE pubkey = $1`,
		} {
			_, _ = db.Exec(q, pubkey)
		}
	})
}

func activeAddressCount(t *testing.T, db *sql.DB, pubkey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE`, pubkey).Scan(&n); err != nil {
		t.Fatalf("count addresses: %v", err)
	}
	return n
}

func TestFirstAddressesMe_ReturnsTheAddress(t *testing.T) {
	r, db := ensureTestRouter(t)
	sk := nostr.GeneratePrivateKey()
	pk, _ := nostr.GetPublicKey(sk)
	cleanupEnsurePubkey(t, db, pk)

	const u = "http://example.com/api/v1/addresses/me"
	req := httptest.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", signLikeCLI(t, sk, "GET", u, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("brand-new pubkey's FIRST /addresses/me = %d %s; want 200 with its address", w.Code, w.Body.String())
	}
	var got AddressResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Username == "" || got.Pubkey != pk || !got.Verified || !got.IsPrimary {
		t.Fatalf("got %+v; want a verified primary address for %s", got, pk[:8])
	}
	if n := activeAddressCount(t, db, pk); n != 1 {
		t.Fatalf("pubkey has %d active addresses; want exactly 1", n)
	}
}
