package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"git.aegis-hq.xyz/coldforge/cloistr-common/username"
)

// These are DB-integration tests for the identity auto-provision + primary-promotion
// logic. They run only when TEST_DATABASE_URL points at a Postgres that has the base
// schema + migration 007 applied (effective_quota fns, addresses.auto_assigned). They
// use random pubkeys and clean up after themselves so they are safe against a shared
// scratch database.
//
//	TEST_DATABASE_URL='postgres://postgres:x@172.17.0.2:5432/cloistr?sslmode=disable' go test ./internal/storage/ -run Identity -v
func testStore(t *testing.T) (*Storage, func()) {
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
	return &Storage{db: db}, func() { db.Close() }
}

func randPubkey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func cleanupPubkey(t *testing.T, s *Storage, pubkey string) {
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM address_ownership WHERE pubkey = $1`, pubkey)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM addresses WHERE pubkey = $1`, pubkey)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM users WHERE pubkey = $1`, pubkey)
}

func TestIdentityEnsureUserIdempotent(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	defer cleanupPubkey(t, s, pk)

	if err := s.EnsureUser(ctx, pk); err != nil {
		t.Fatalf("EnsureUser #1: %v", err)
	}
	if err := s.EnsureUser(ctx, pk); err != nil {
		t.Fatalf("EnsureUser #2 (idempotent): %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE pubkey = $1`, pk).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("users rows = %d, want 1", n)
	}
}

func TestIdentityEnsureAutoAddress(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	defer cleanupPubkey(t, s, pk)

	if err := s.EnsureUser(ctx, pk); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	addr, err := s.EnsureAutoAddress(ctx, pk, "cloistr.xyz")
	if err != nil {
		t.Fatalf("EnsureAutoAddress: %v", err)
	}
	if addr == nil {
		t.Fatal("EnsureAutoAddress returned nil for a nameless pubkey")
	}
	if !username.IsAutoAssigned(addr.Username) {
		t.Errorf("generated %q is not the auto-assigned shape", addr.Username)
	}
	if !addr.IsPrimary || !addr.NIP05Active {
		t.Errorf("first (auto) address should be primary+nip05: primary=%v nip05=%v", addr.IsPrimary, addr.NIP05Active)
	}
	var autoFlag bool
	if err := s.db.QueryRowContext(ctx, `SELECT auto_assigned FROM addresses WHERE id = $1`, addr.ID).Scan(&autoFlag); err != nil {
		t.Fatal(err)
	}
	if !autoFlag {
		t.Error("auto_assigned should be TRUE")
	}

	// Second call is a no-op (already has an active address).
	addr2, err := s.EnsureAutoAddress(ctx, pk, "cloistr.xyz")
	if err != nil {
		t.Fatalf("EnsureAutoAddress #2: %v", err)
	}
	if addr2 != nil {
		t.Errorf("EnsureAutoAddress #2 should be a no-op, got %v", addr2.Username)
	}
}

func TestIdentityPrimaryPromotion(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	defer cleanupPubkey(t, s, pk)

	if err := s.EnsureUser(ctx, pk); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	auto, err := s.EnsureAutoAddress(ctx, pk, "cloistr.xyz")
	if err != nil || auto == nil {
		t.Fatalf("EnsureAutoAddress: %v addr=%v", err, auto)
	}

	// Claim a real human name: it must become primary+nip05 and demote the auto address.
	human := "realname" + randPubkey(t)[:6]
	claimed, err := s.AtomicRegisterAddress(ctx, human, "cloistr.xyz", pk, false)
	if err != nil {
		t.Fatalf("AtomicRegisterAddress(human): %v", err)
	}
	if claimed == nil {
		t.Fatal("claimed human name returned nil (taken/reserved?)")
	}
	if !claimed.IsPrimary || !claimed.NIP05Active {
		t.Errorf("claimed name should be promoted to primary+nip05: primary=%v nip05=%v", claimed.IsPrimary, claimed.NIP05Active)
	}

	// The auto address must still be active but demoted.
	var stillActive, isPrimary, nip05 bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT active, is_primary, nip05_active FROM addresses WHERE id = $1`, auto.ID,
	).Scan(&stillActive, &isPrimary, &nip05); err != nil {
		t.Fatal(err)
	}
	if !stillActive {
		t.Error("auto address should be kept active (alias), not deactivated")
	}
	if isPrimary || nip05 {
		t.Errorf("auto address should be demoted: primary=%v nip05=%v", isPrimary, nip05)
	}

	// Exactly one primary and one nip05_active remain.
	var primaries, nip05s int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM addresses WHERE pubkey=$1 AND active AND is_primary`, pk).Scan(&primaries)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM addresses WHERE pubkey=$1 AND active AND nip05_active`, pk).Scan(&nip05s)
	if primaries != 1 || nip05s != 1 {
		t.Errorf("want exactly one primary and one nip05, got primaries=%d nip05s=%d", primaries, nip05s)
	}
}

func TestIdentityPlainAliasNoPromotion(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	defer cleanupPubkey(t, s, pk)

	if err := s.EnsureUser(ctx, pk); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	// First real name -> primary.
	first := "firstname" + randPubkey(t)[:6]
	a1, err := s.AtomicRegisterAddress(ctx, first, "cloistr.xyz", pk, false)
	if err != nil || a1 == nil {
		t.Fatalf("register first: %v addr=%v", err, a1)
	}
	if !a1.IsPrimary {
		t.Fatal("first real name should be primary")
	}
	// Second real name -> plain alias, primary unchanged.
	second := "secondname" + randPubkey(t)[:6]
	a2, err := s.AtomicRegisterAddress(ctx, second, "cloistr.xyz", pk, false)
	if err != nil || a2 == nil {
		t.Fatalf("register second: %v addr=%v", err, a2)
	}
	if a2.IsPrimary || a2.NIP05Active {
		t.Errorf("second real name should be a plain alias: primary=%v nip05=%v", a2.IsPrimary, a2.NIP05Active)
	}
	var stillPrimary bool
	_ = s.db.QueryRowContext(ctx, `SELECT is_primary FROM addresses WHERE id=$1`, a1.ID).Scan(&stillPrimary)
	if !stillPrimary {
		t.Error("original primary should remain primary when a plain alias is added")
	}
}
