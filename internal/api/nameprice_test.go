package api

import (
	"context"
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

/*
One free real name per account.

Before this rule, price came from get_username_price(length) alone — no pubkey
anywhere in the path — so a single key could claim unlimited free 6+ names and
squatting the namespace cost nothing.

The auto-assigned address matters here: the NIP-98 middleware hands one to every
authenticated pubkey, so counting ALL addresses would charge everybody for their
very first real name.
*/

func priceHandlerWithMock(t *testing.T) (*Handler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Handler{cfg: &config.Config{}, store: storage.NewWithDB(db)}, mock
}

func expectTierLookup(mock sqlmock.Sqlmock, price int64, tier string) {
	mock.ExpectQuery("get_username_price").
		WillReturnRows(sqlmock.NewRows([]string{"get_username_price"}).AddRow(price))
	mock.ExpectQuery("tier_name").
		WillReturnRows(sqlmock.NewRows([]string{"tier_name"}).AddRow(tier))
}

func TestPriceNameFor_FirstRealNameIsFree(t *testing.T) {
	h, mock := priceHandlerWithMock(t)
	expectTierLookup(mock, 0, "standard")
	// Auto-assigned addresses are excluded by the query, so a brand-new account
	// that already has its adjective-noun-NNNN address still counts zero.
	mock.ExpectQuery("COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	got, err := h.priceNameFor(context.Background(), "coldforge", "pubkey-a")
	if err != nil {
		t.Fatalf("priceNameFor: %v", err)
	}
	if got.PriceSats != 0 || got.Additional {
		t.Fatalf("first name = %+v, want free and not additional", got)
	}
}

func TestPriceNameFor_SecondFreeTierNameIsCharged(t *testing.T) {
	// THE BUG. This is the case that used to return 0 forever.
	h, mock := priceHandlerWithMock(t)
	expectTierLookup(mock, 0, "standard")
	mock.ExpectQuery("COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("price_sats FROM products").
		WillReturnRows(sqlmock.NewRows([]string{"price_sats"}).AddRow(int64(1000)))

	got, err := h.priceNameFor(context.Background(), "coldforge", "pubkey-a")
	if err != nil {
		t.Fatalf("priceNameFor: %v", err)
	}
	if got.PriceSats != 1000 {
		t.Fatalf("second name price = %d, want 1000", got.PriceSats)
	}
	if !got.Additional {
		t.Fatal("second name must be flagged Additional so the UI can explain the charge")
	}
	if got.Tier != "standard" {
		t.Fatalf("tier = %q, want the LENGTH tier %q — the charge is not a tier change", got.Tier, "standard")
	}
}

func TestPriceNameFor_PaidTierIsUnaffectedByTheAllowance(t *testing.T) {
	// A short name was never part of the free allowance, so holding a name
	// must not change its price — and must not cost an extra COUNT query.
	h, mock := priceHandlerWithMock(t)
	expectTierLookup(mock, 5000, "short")

	got, err := h.priceNameFor(context.Background(), "abcde", "pubkey-a")
	if err != nil {
		t.Fatalf("priceNameFor: %v", err)
	}
	if got.PriceSats != 5000 || got.Additional {
		t.Fatalf("short name = %+v, want 5000 and not additional", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("paid tier should not query the address count: %v", err)
	}
}

func TestPriceNameFor_AnonymousCallerGetsTheTierPrice(t *testing.T) {
	// The availability endpoint is open. With no pubkey the honest answer is
	// what a NEW account would pay, and no address count is possible.
	h, mock := priceHandlerWithMock(t)
	expectTierLookup(mock, 0, "standard")

	got, err := h.priceNameFor(context.Background(), "coldforge", "")
	if err != nil {
		t.Fatalf("priceNameFor: %v", err)
	}
	if got.PriceSats != 0 || got.Additional {
		t.Fatalf("anonymous = %+v, want the free tier price", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("anonymous pricing should not query an address count: %v", err)
	}
}

func TestPriceNameFor_MissingProductChargesFallbackNotZero(t *testing.T) {
	// If the catalog row is missing, falling back to 0 would silently restore
	// unlimited free names and look like it was working.
	h, mock := priceHandlerWithMock(t)
	expectTierLookup(mock, 0, "standard")
	mock.ExpectQuery("COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery("price_sats FROM products").WillReturnError(sqlmock.ErrCancelled)

	_, err := h.priceNameFor(context.Background(), "coldforge", "pubkey-a")
	if err == nil {
		t.Fatal("a real product-lookup failure must be an error, not a guess")
	}
}

func TestPriceNameFor_ProductAbsentUsesDocumentedFallback(t *testing.T) {
	h, mock := priceHandlerWithMock(t)
	expectTierLookup(mock, 0, "standard")
	mock.ExpectQuery("COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("price_sats FROM products").WillReturnError(errNoRowsForTest())

	got, err := h.priceNameFor(context.Background(), "coldforge", "pubkey-a")
	if err != nil {
		t.Fatalf("priceNameFor: %v", err)
	}
	if got.PriceSats != fallbackAdditionalPriceSats {
		t.Fatalf("fallback price = %d, want %d — never 0", got.PriceSats, fallbackAdditionalPriceSats)
	}
	if !got.Additional {
		t.Fatal("fallback must still be flagged Additional")
	}
}

// errNoRowsForTest is the driver error the storage layer maps to
// ErrProductNotFound. It must be sql.ErrNoRows itself: any other error is a
// genuine lookup failure and must NOT reach the fallback.
func errNoRowsForTest() error { return sql.ErrNoRows }
