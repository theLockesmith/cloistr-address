package storage

import (
	"context"
	"strings"
	"testing"
)

// TestEnsureUserFunction covers the public.ensure_user() SQL function from
// migration 011, which services without INSERT on users (blossom, signer,
// relay) call to create a bare platform user row. CI applies 011 on top of
// db/test_schema.sql. The privilege boundary itself (SECURITY DEFINER, grants)
// needs non-superuser roles and was verified against a production-shaped
// database when the migration was written.
func TestEnsureUserFunction(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()

	fresh := randPubkey(t)
	defer cleanupPubkey(t, s, fresh)
	for i := 0; i < 2; i++ {
		if _, err := s.db.ExecContext(ctx, `SELECT public.ensure_user($1)`, fresh); err != nil {
			t.Fatalf("ensure_user call %d: %v", i+1, err)
		}
	}
	var enabled, admin bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT enabled, is_platform_admin FROM users WHERE pubkey = $1`, fresh).Scan(&enabled, &admin); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !enabled || admin {
		t.Errorf("new row: enabled=%v is_platform_admin=%v, want column defaults true/false", enabled, admin)
	}

	disabled := randPubkey(t)
	defer cleanupPubkey(t, s, disabled)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO users (pubkey, enabled) VALUES ($1, FALSE)`, disabled); err != nil {
		t.Fatalf("seed disabled user: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `SELECT public.ensure_user($1)`, disabled); err != nil {
		t.Fatalf("ensure_user on disabled: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT enabled FROM users WHERE pubkey = $1`, disabled).Scan(&enabled); err != nil {
		t.Fatalf("read disabled: %v", err)
	}
	if enabled {
		t.Error("ensure_user re-enabled a disabled user")
	}

	for _, bad := range []string{strings.ToUpper(randPubkey(t)), "abc123", " " + fresh[1:], fresh + "0"} {
		if _, err := s.db.ExecContext(ctx, `SELECT public.ensure_user($1)`, bad); err == nil {
			t.Errorf("ensure_user(%q) succeeded, want rejection", bad)
		}
	}
	if _, err := s.db.ExecContext(ctx, `SELECT public.ensure_user(NULL)`); err == nil {
		t.Error("ensure_user(NULL) succeeded, want rejection")
	}
}
