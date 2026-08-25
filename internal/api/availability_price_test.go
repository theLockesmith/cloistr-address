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
Zero is a REAL price here — names of 6+ characters are free by design — so the
availability endpoint cannot use it as a "no value" marker. It used to: a failed
`get_username_price` was logged at warn and the response went out with
price_sats: 0, which the UI renders as free. The user then clicked "Claim Free"
and got "Insufficient credits" from a code path that had never agreed the name
was free.

These tests pin the distinction: genuinely free answers 0, a broken lookup
answers 500.
*/

func priceHandler(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{}, store: storage.NewWithDB(db)}
	r := gin.New()
	r.GET("/api/v1/addresses/check/:username", h.checkUsernameAvailability)
	return r, mock
}

func TestCheckUsernameAvailability_FreeNameQuotesZero(t *testing.T) {
	r, mock := priceHandler(t)
	mock.ExpectQuery("is_username_available").
		WillReturnRows(sqlmock.NewRows([]string{"is_username_available"}).AddRow(true))
	mock.ExpectQuery("get_username_price").
		WillReturnRows(sqlmock.NewRows([]string{"get_username_price"}).AddRow(int64(0)))
	mock.ExpectQuery("tier_name").
		WillReturnRows(sqlmock.NewRows([]string{"tier_name"}).AddRow("standard"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/addresses/check/coldforge", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var got UsernameAvailabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Available {
		t.Fatalf("available = false, want true")
	}
	if got.PriceSats != 0 {
		t.Fatalf("price_sats = %d, want 0 (free tier)", got.PriceSats)
	}
	if got.Tier != "standard" {
		t.Fatalf("tier = %q, want %q", got.Tier, "standard")
	}
}

func TestCheckUsernameAvailability_PriceLookupFailureIsNotFree(t *testing.T) {
	r, mock := priceHandler(t)
	mock.ExpectQuery("is_username_available").
		WillReturnRows(sqlmock.NewRows([]string{"is_username_available"}).AddRow(true))
	mock.ExpectQuery("get_username_price").WillReturnError(errors.New("connection reset by peer"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/addresses/check/coldforge", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	// Specifically: it must not have answered 200 with a price of zero.
	if body := w.Body.String(); body != "" {
		var got UsernameAvailabilityResponse
		if err := json.Unmarshal([]byte(body), &got); err == nil && got.Available {
			t.Fatalf("reported an available name despite an unknown price: %s", body)
		}
	}
}

func TestCheckUsernameAvailability_TierLookupFailureIsNotFree(t *testing.T) {
	r, mock := priceHandler(t)
	mock.ExpectQuery("is_username_available").
		WillReturnRows(sqlmock.NewRows([]string{"is_username_available"}).AddRow(true))
	mock.ExpectQuery("get_username_price").
		WillReturnRows(sqlmock.NewRows([]string{"get_username_price"}).AddRow(int64(0)))
	mock.ExpectQuery("tier_name").WillReturnError(errors.New("connection reset by peer"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/addresses/check/coldforge", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
}
