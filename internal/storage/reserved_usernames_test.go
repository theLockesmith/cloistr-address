package storage

import (
	"context"
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
