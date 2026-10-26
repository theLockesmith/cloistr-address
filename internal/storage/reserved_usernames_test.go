package storage

import (
	"context"
	"os"
	"testing"
)

// TestIsUsernameAvailable_SystemReservedNames is a DB-integration test (see
// testStore in postgres_identity_test.go for the TEST_DATABASE_URL skip/setup
// convention).
//
// ME-7: GET /api/v1/addresses/check/admin answered "Available!" in production.
// is_username_available() already refuses anything in reserved_usernames with
// reserved_for_pubkey IS NULL -- the bug was that nothing had ever inserted the
// common system words there (migration 014 does). This fails on a database
// without migration 014 applied and passes once it is.
func TestIsUsernameAvailable_SystemReservedNames(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()

	for _, name := range []string{"admin", "root", "support", "ADMIN"} {
		available, err := s.IsUsernameAvailable(ctx, name)
		if err != nil {
			t.Fatalf("IsUsernameAvailable(%q): %v", name, err)
		}
		if available {
			t.Errorf("IsUsernameAvailable(%q) = true, want false (system reserved)", name)
		}
	}
}

// The init container reapplies every *_up.sql on each pod start, and freeing a
// reserved name through the admin API (RemoveReserved) DELETEs its row. So 014
// must seed once: a rerun must not re-reserve a name an operator freed.
func TestMigration014_RerunKeepsFreedNameFree(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()

	sqlBytes, err := os.ReadFile("../../db/migrations/014_up.sql")
	if err != nil {
		t.Fatal(err)
	}
	// CI has applied 014 already; apply it here too so the test stands alone.
	if _, err := s.db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("014: %v", err)
	}
	const name = "webmaster"
	if available, err := s.IsUsernameAvailable(ctx, name); err != nil || available {
		t.Fatalf("after 014, IsUsernameAvailable(%q) = (%v, %v), want (false, nil)", name, available, err)
	}
	defer func() {
		_, _ = s.db.ExecContext(ctx, `INSERT INTO reserved_usernames (username, reason) VALUES ($1, 'system reserved') ON CONFLICT DO NOTHING`, name)
	}()

	// An operator frees it, the way RemoveReserved does.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM reserved_usernames WHERE username = $1`, name); err != nil {
		t.Fatal(err)
	}
	// Next pod start.
	if _, err := s.db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("014 rerun: %v", err)
	}
	available, err := s.IsUsernameAvailable(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatalf("014 rerun re-reserved %q after an operator freed it", name)
	}
}
