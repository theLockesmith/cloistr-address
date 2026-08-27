package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

/*
The signup page's price table was hardcoded JSX claiming a 6+ character name
costs 1,000 sats. Migration 006 set username_tiers.standard to 0 — the first 6+
name is FREE — and 1,000 sats is address_standard_additional, a SECOND name. So
the page overcharged every new visitor in its marketing copy.

These tests pin the two halves that made it wrong: the free tier must survive as
a real 0, and the additional-name price must travel with it. Either one alone is
still a lie.
*/

func pricingRouter(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{}, store: storage.NewWithDB(db)}
	r := gin.New()
	r.GET("/api/v1/pricing/tiers", h.listPricingTiers)
	return r, mock
}

func tierRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "tier_name", "min_length", "max_length", "price_sats", "enabled"})
}

func getTiers(t *testing.T, r *gin.Engine) (int, PricingTiersResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/pricing/tiers", nil))
	var got PricingTiersResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v; body = %s", err, w.Body.String())
		}
	}
	return w.Code, got
}

// THE BUG. The page said 1,000 sats for a 6+ name; the catalog says 0, with
// 1,000 applying only to a second one.
func TestListPricingTiers_StandardTierIsFreeAndCarriesAdditionalPrice(t *testing.T) {
	r, mock := pricingRouter(t)
	mock.ExpectQuery("FROM username_tiers").WillReturnRows(
		tierRows().AddRow(1, "standard", 6, nil, int64(0), true))
	mock.ExpectQuery("price_sats FROM products").
		WillReturnRows(sqlmock.NewRows([]string{"price_sats"}).AddRow(int64(1000)))

	code, got := getTiers(t, r)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Tiers) != 1 {
		t.Fatalf("tiers = %+v, want exactly one", got.Tiers)
	}
	if got.Tiers[0].PriceSats != 0 {
		t.Errorf("standard tier = %d sats, want 0 (the first 6+ name is free)", got.Tiers[0].PriceSats)
	}
	if got.Tiers[0].MaxLength != nil {
		t.Errorf("max_length = %v, want null so the UI can render \"6+\"", *got.Tiers[0].MaxLength)
	}
	if got.AdditionalAddressSats != 1000 {
		t.Errorf("additional = %d, want 1000; a free tier without it is half the truth",
			got.AdditionalAddressSats)
	}
}

func TestListPricingTiers_OmitsDisabledTiers(t *testing.T) {
	r, mock := pricingRouter(t)
	two := 2
	mock.ExpectQuery("FROM username_tiers").WillReturnRows(
		tierRows().
			AddRow(1, "ultra_premium", 1, &two, int64(50000), false).
			AddRow(2, "standard", 6, nil, int64(0), true))
	mock.ExpectQuery("price_sats FROM products").
		WillReturnRows(sqlmock.NewRows([]string{"price_sats"}).AddRow(int64(1000)))

	code, got := getTiers(t, r)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// A disabled tier is not for sale. Advertising it quotes a price the
	// purchase path will refuse.
	if len(got.Tiers) != 1 || got.Tiers[0].Tier != "standard" {
		t.Fatalf("tiers = %+v, want only the enabled one", got.Tiers)
	}
}

// Half a price table renders as a whole one — the missing row leaves no gap a
// reader would notice — so a broken lookup must refuse rather than serve what
// it happened to fetch.
func TestListPricingTiers_TierLookupFailureIs500(t *testing.T) {
	r, mock := pricingRouter(t)
	mock.ExpectQuery("FROM username_tiers").WillReturnError(errors.New("db down"))

	if code, _ := getTiers(t, r); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

// A missing catalog row must not become "additional names are free", which is
// what a zero here would render as. Mirrors priceNameFor's fallback.
func TestListPricingTiers_MissingAdditionalProductFallsBackNotFree(t *testing.T) {
	r, mock := pricingRouter(t)
	mock.ExpectQuery("FROM username_tiers").WillReturnRows(
		tierRows().AddRow(1, "standard", 6, nil, int64(0), true))
	mock.ExpectQuery("price_sats FROM products").WillReturnError(errNoRowsForTest())

	code, got := getTiers(t, r)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.AdditionalAddressSats != fallbackAdditionalPriceSats {
		t.Errorf("additional = %d, want the %d fallback rather than a free-looking 0",
			got.AdditionalAddressSats, fallbackAdditionalPriceSats)
	}
}

// Every tier disabled is a real state. It must serialise as [] and not null, or
// a client mapping over it throws instead of rendering an empty table.
func TestListPricingTiers_EmptyListIsArrayNotNull(t *testing.T) {
	r, mock := pricingRouter(t)
	mock.ExpectQuery("FROM username_tiers").WillReturnRows(tierRows())
	mock.ExpectQuery("price_sats FROM products").
		WillReturnRows(sqlmock.NewRows([]string{"price_sats"}).AddRow(int64(1000)))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/pricing/tiers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["tiers"]) != "[]" {
		t.Errorf("tiers = %s, want []", raw["tiers"])
	}
}
