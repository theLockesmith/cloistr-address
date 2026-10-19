package storage

import (
	"context"
	"os"
	"testing"
)

// cloistr-email refuses to send from (or open mail for) an address whose
// addresses.verified is false: identity.ValidateSender and the alias branch of
// ResolveFromAddress both return "unified address not verified". cloistr-me is
// the only writer of registered addresses, and the address belongs to the
// pubkey that authenticated (or paid) for it, so every registration path must
// store verified=true.

// emailSenderVerified runs cloistr-email's sender lookup (GetAddressByPubkey in
// cloistr-email internal/storage/postgres.go) and returns the verified flag it
// would check.
func emailSenderVerified(t *testing.T, s *Storage, pubkey string) bool {
	t.Helper()
	var verified bool
	err := s.db.QueryRowContext(context.Background(), `
		SELECT verified
		FROM addresses
		WHERE pubkey = $1 AND active = true
		ORDER BY is_primary DESC NULLS LAST, id ASC
		LIMIT 1
	`, pubkey).Scan(&verified)
	if err != nil {
		t.Fatalf("email sender lookup: %v", err)
	}
	return verified
}

func TestAddressVerified_AutoAddressCanSendEmail(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	defer cleanupPubkey(t, s, pk)

	if err := s.EnsureUser(ctx, pk); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	addr, err := s.EnsureAutoAddress(ctx, pk, "cloistr.xyz")
	if err != nil || addr == nil {
		t.Fatalf("EnsureAutoAddress: %v addr=%v", err, addr)
	}
	if !addr.Verified {
		t.Error("EnsureAutoAddress returned verified=false")
	}
	if !emailSenderVerified(t, s, pk) {
		t.Fatal("auto address is stored verified=false; cloistr-email refuses to send from it")
	}
}

func TestAddressVerified_PurchasedAddressCanSendEmail(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	defer cleanupPubkey(t, s, pk)

	if err := s.EnsureUser(ctx, pk); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	// The auto address comes first, then the bought name is promoted to primary,
	// which is what the sender lookup picks.
	if _, err := s.EnsureAutoAddress(ctx, pk, "cloistr.xyz"); err != nil {
		t.Fatalf("EnsureAutoAddress: %v", err)
	}
	addr, err := s.AtomicRegisterAddress(ctx, "bought"+randPubkey(t)[:6], "cloistr.xyz", pk, false)
	if err != nil || addr == nil {
		t.Fatalf("AtomicRegisterAddress: %v addr=%v", err, addr)
	}
	if !addr.Verified {
		t.Error("AtomicRegisterAddress returned verified=false")
	}
	if !emailSenderVerified(t, s, pk) {
		t.Fatal("purchased address is stored verified=false; cloistr-email refuses to send from it")
	}
	// Sending from any owned alias checks that alias's own flag.
	var unverified int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM addresses WHERE pubkey = $1 AND active AND NOT verified`, pk).Scan(&unverified); err != nil {
		t.Fatal(err)
	}
	if unverified != 0 {
		t.Fatalf("%d of this pubkey's addresses are verified=false; email refuses to send from them", unverified)
	}
}

func TestAddressVerified_LegacyRegisterCanSendEmail(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	defer cleanupPubkey(t, s, pk)

	if err := s.EnsureUser(ctx, pk); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	addr, err := s.RegisterAddress(ctx, "legacy"+randPubkey(t)[:6], "cloistr.xyz", pk)
	if err != nil {
		t.Fatalf("RegisterAddress: %v", err)
	}
	if !addr.Verified || !emailSenderVerified(t, s, pk) {
		t.Fatal("RegisterAddress stored verified=false; cloistr-email refuses to send from it")
	}
}

// Migration 013 backfills rows written before the fix. The init container
// reapplies every migration on each pod start, so it must run once: a second
// application leaves alone a row someone deliberately set back to false.
func TestMigration013_BackfillsOnce(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	defer cleanupPubkey(t, s, pk)

	sqlBytes, err := os.ReadFile("../../db/migrations/013_up.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Start from a database where 013 has not run yet.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM me_data_migrations WHERE name = '013_addresses_verified_backfill'`); err != nil {
		t.Logf("no marker table yet (first run): %v", err)
	}

	if err := s.EnsureUser(ctx, pk); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO addresses (username, domain, pubkey, active, verified, is_primary, nip05_active)
		VALUES ($1, 'cloistr.xyz', $2, true, false, true, true)
	`, "prefix"+pk[:6], pk); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("013 first run: %v", err)
	}
	if !emailSenderVerified(t, s, pk) {
		t.Fatal("013 did not backfill a verified=false row")
	}

	// An operator undoes it for this row; the next pod start must not redo it.
	if _, err := s.db.ExecContext(ctx, `UPDATE addresses SET verified = false WHERE pubkey = $1`, pk); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("013 rerun: %v", err)
	}
	if emailSenderVerified(t, s, pk) {
		t.Fatal("013 rerun flipped a deliberately unverified row back to true")
	}
}
