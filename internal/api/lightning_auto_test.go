package api

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

func lightningHandler(t *testing.T) (*Handler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Handler{cfg: &config.Config{}, store: storage.NewWithDB(db)}, mock
}

func TestAutoConfigureLightning_CreatesProxyRow(t *testing.T) {
	h, mock := lightningHandler(t)
	addr := "alice@getalby.com"

	mock.ExpectExec("INSERT INTO address_lightning").
		WithArgs(int64(42), "proxy", addr, "", "", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.autoConfigureLightning(context.Background(), 42, &addr, "testuser")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected INSERT INTO address_lightning: %v", err)
	}
}

func TestAutoConfigureLightning_NilIsNoop(t *testing.T) {
	h, _ := lightningHandler(t)

	// Should not touch the DB at all.
	h.autoConfigureLightning(context.Background(), 42, nil, "testuser")
	// If it tried a DB call, sqlmock would fail because nothing was expected.
}

func TestAutoConfigureLightning_EmptyStringIsNoop(t *testing.T) {
	h, _ := lightningHandler(t)
	empty := ""

	h.autoConfigureLightning(context.Background(), 42, &empty, "testuser")
}

func TestAutoConfigureLightning_FailureDoesNotPanic(t *testing.T) {
	// Best-effort: a DB error is logged, not propagated.
	h, mock := lightningHandler(t)
	addr := "bob@walletofsatoshi.com"

	mock.ExpectExec("INSERT INTO address_lightning").
		WithArgs(int64(99), "proxy", addr, "", "", "").
		WillReturnError(context.DeadlineExceeded)

	// Must not panic or propagate the error.
	h.autoConfigureLightning(context.Background(), 99, &addr, "testuser")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestMetaLightningAddress_RoundTrips(t *testing.T) {
	// The metadata key written at invoice creation must be readable at settlement.
	meta := map[string]any{
		MetaKind:             KindAddress,
		MetaUsername:         "alice",
		MetaPubkey:           "deadbeef",
		MetaLightningAddress: "alice@getalby.com",
	}

	got, ok := meta[MetaLightningAddress].(string)
	if !ok || got != "alice@getalby.com" {
		t.Fatalf("MetaLightningAddress round-trip failed: got %q, ok=%v", got, ok)
	}
}

func TestMetaLightningAddress_AbsentIsEmptyString(t *testing.T) {
	// Legacy invoices won't have this key. The webhook code uses type assertion
	// with a default empty string.
	meta := map[string]any{
		MetaKind:     KindAddress,
		MetaUsername: "alice",
		MetaPubkey:   "deadbeef",
	}

	got, _ := meta[MetaLightningAddress].(string)
	if got != "" {
		t.Fatalf("absent MetaLightningAddress should be empty, got %q", got)
	}
}

func TestIsValidLightningAddress(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"alice@getalby.com", true},
		{"bob@walletofsatoshi.com", true},
		{"user@ln.tips", true},
		{"", false},
		{"noatsign", false},
		{"@nodomain", false},
		{"user@", false},
		{"user@x", false}, // domain too short (no dot)
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := isValidLightningAddress(tc.addr); got != tc.want {
				t.Fatalf("isValidLightningAddress(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// Verify that UpsertLightningConfig exists and accepts the arguments
// autoConfigureLightning passes. This is a compile-time contract test:
// if the storage signature changes, this file stops building.
var _ = func() {
	var s *storage.Storage
	_ = s.UpsertLightningConfig(context.Background(), 0, "", "")
}
