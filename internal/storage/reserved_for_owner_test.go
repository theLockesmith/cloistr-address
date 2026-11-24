package storage

import (
	"context"
	"testing"
)

// A name reserved for one pubkey (reserved_usernames.reserved_for_pubkey set,
// via the admin "reserve add --for") showed as available to everyone, because
// is_username_available() only refuses reservations with no owner. Anyone
// could then pay for it, and AtomicRegisterAddress refused every reserved row,
// the owner's included, so every purchase was refunded and the owner could
// never claim it.

func reserveFor(t *testing.T, s *Storage, name, pubkey string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO reserved_usernames (username, reserved_for_pubkey, reason) VALUES ($1, $2, 'test')
		 ON CONFLICT (username) DO UPDATE SET reserved_for_pubkey = $2`, name, pubkey); err != nil {
		t.Fatalf("reserve %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM reserved_usernames WHERE username = $1`, name)
	})
}

func TestReservedForOwner_NotAvailableToEveryoneElse(t *testing.T) {
	s, done := testStore(t)
	defer done()
	owner := randPubkey(t)
	const name = "heldforowner"
	reserveFor(t, s, name, owner)

	available, err := s.IsUsernameAvailable(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatalf("IsUsernameAvailable(%q) = true for an anonymous visitor, want false (reserved for %s)", name, owner[:8])
	}
}

func TestReservedForOwner_OwnerCanRegister(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	owner, other := randPubkey(t), randPubkey(t)
	defer cleanupPubkey(t, s, owner)
	defer cleanupPubkey(t, s, other)
	const name = "claimbyowner"
	reserveFor(t, s, name, owner)

	if addr, err := s.AtomicRegisterAddress(ctx, name, "cloistr.xyz", other, false); err != nil || addr != nil {
		t.Fatalf("AtomicRegisterAddress by a different pubkey = (%v, %v), want (nil, nil)", addr, err)
	}
	addr, err := s.AtomicRegisterAddress(ctx, name, "cloistr.xyz", owner, false)
	if err != nil {
		t.Fatalf("AtomicRegisterAddress by owner: %v", err)
	}
	if addr == nil {
		t.Fatalf("AtomicRegisterAddress by the reserved pubkey returned nil: the owner can never claim %q", name)
	}
}

func TestReservedForOwner_AvailableToOwnerOnly(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	owner, other := randPubkey(t), randPubkey(t)
	const name = "askedbyowner"
	reserveFor(t, s, name, owner)

	for _, tc := range []struct {
		who    string
		pubkey string
		want   bool
	}{{"owner", owner, true}, {"other pubkey", other, false}, {"anonymous", "", false}} {
		got, err := s.IsUsernameAvailableFor(ctx, name, tc.pubkey)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("IsUsernameAvailableFor(%q) as %s = %v, want %v", name, tc.who, got, tc.want)
		}
	}
}
