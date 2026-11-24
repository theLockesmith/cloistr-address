package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/btcpay"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

/*
A storage top-up bought while the Lightning node was still starting got
"500 Failed to create invoice": BTCPay's invoice create timed out (context
deadline exceeded). The node not answering is the same transient trouble as
BTCPay's "lightning node did not reply" 400, which already answers 503 "Lightning
is temporarily unavailable, please try again". A timeout must answer the same
way, on every path that creates an invoice. Other BTCPay failures stay 500.
*/

const lightningUnavailableBody = `{"error":"Lightning is temporarily unavailable, please try again","retries":true}`

func TestWriteInvoiceError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"lightning unavailable", fmt.Errorf("x: %w", btcpay.ErrPaymentMethodUnavailable), http.StatusServiceUnavailable, lightningUnavailableBody},
		{"timeout", fmt.Errorf("btcpay create invoice: %w", btcpay.ErrInvoiceTimeout), http.StatusServiceUnavailable, lightningUnavailableBody},
		{"other btcpay failure", errors.New("BTCPay returned status 401: nope"), http.StatusInternalServerError, `{"error":"Failed to create invoice"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			writeInvoiceError(c, tc.err)
			if w.Code != tc.wantCode || w.Body.String() != tc.wantBody {
				t.Errorf("got %d %s; want %d %s", w.Code, w.Body.String(), tc.wantCode, tc.wantBody)
			}
		})
	}
}

// End to end on the product path, with a BTCPay that never answers.
func TestCreateProductInvoice_BTCPayTimeoutIs503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer func() { close(release); hung.Close() }()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery("FROM products").WillReturnRows(
		sqlmock.NewRows([]string{"id", "display_name", "price_sats", "product_type", "billing_period", "grants_quota_increases", "enabled"}).
			AddRow("storage_topup_10", "Storage +10 GiB (30d)", int64(500), "storage_topup", "", []byte(`{"storage_bytes":10737418240}`), true))

	h := &Handler{
		cfg:    &config.Config{},
		store:  storage.NewWithDB(db),
		btcpay: btcpay.NewClient(config.BTCPayConfig{URL: hung.URL, APIKey: "k", StoreID: "store1"}),
	}

	// The request's own deadline stands in for the client's per-attempt one,
	// so the test takes a fraction of a second rather than the real budget.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"product_id": "storage_topup_10"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/purchase/product", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	h.createProductInvoice(c, buyerPubkey)

	if w.Code != http.StatusServiceUnavailable || w.Body.String() != lightningUnavailableBody {
		t.Fatalf("got %d %s; want 503 %s", w.Code, w.Body.String(), lightningUnavailableBody)
	}
}
