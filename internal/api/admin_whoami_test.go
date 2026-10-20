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
admin-ui asks GET /admin/v1/me to decide whether to show the console. For a
signed-in user who is not a platform admin it answered 403, so every non-admin
visit logged a failed request and web verification counted the page as broken.
"Am I an admin?" has a normal answer for them: 200 {is_platform_admin:false}.
Unauthenticated stays 401, and every other /admin/v1 route stays 403.
*/

func whoamiRouter(t *testing.T) (http.Handler, *sql.DB) {
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
	return NewHandler(&config.Config{Domain: "cloistr.xyz"}, storage.NewWithDB(db)).Router(), db
}

func newUser(t *testing.T, db *sql.DB, admin bool) (sk, pk string) {
	t.Helper()
	sk = nostr.GeneratePrivateKey()
	pk, _ = nostr.GetPublicKey(sk)
	if _, err := db.Exec(`INSERT INTO users (pubkey, is_platform_admin) VALUES ($1, $2)`, pk, admin); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE pubkey = $1`, pk) })
	return sk, pk
}

func adminGet(t *testing.T, r http.Handler, sk, path string) *httptest.ResponseRecorder {
	t.Helper()
	u := "http://example.com" + path
	req := httptest.NewRequest("GET", u, nil)
	if sk != "" {
		req.Header.Set("Authorization", signLikeCLI(t, sk, "GET", u, nil))
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAdminWhoami_NonAdminGetsFalse(t *testing.T) {
	r, db := whoamiRouter(t)
	sk, pk := newUser(t, db, false)

	w := adminGet(t, r, sk, "/admin/v1/me")
	if w.Code != http.StatusOK {
		t.Fatalf("non-admin GET /admin/v1/me = %d %s; want 200 {is_platform_admin:false}", w.Code, w.Body.String())
	}
	var got struct {
		Pubkey          string `json:"pubkey"`
		IsPlatformAdmin *bool  `json:"is_platform_admin"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.IsPlatformAdmin == nil || *got.IsPlatformAdmin || got.Pubkey != pk {
		t.Fatalf("body = %s; want is_platform_admin=false for %s", w.Body.String(), pk[:8])
	}

	for _, p := range []string{"/admin/v1/reserved", "/admin/v1/tiers", "/admin/v1/audit", "/admin/v1/addresses"} {
		if w := adminGet(t, r, sk, p); w.Code != http.StatusForbidden {
			t.Errorf("non-admin GET %s = %d; must stay 403", p, w.Code)
		}
	}
}

func TestAdminWhoami_AdminAndUnauthenticated(t *testing.T) {
	r, db := whoamiRouter(t)
	sk, _ := newUser(t, db, true)

	w := adminGet(t, r, sk, "/admin/v1/me")
	if w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("admin GET /admin/v1/me = %d %s; want 200", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["is_platform_admin"] != true {
		t.Fatalf("admin body = %s; want is_platform_admin=true", w.Body.String())
	}

	if w := adminGet(t, r, "", "/admin/v1/me"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /admin/v1/me = %d; must stay 401", w.Code)
	}
}
