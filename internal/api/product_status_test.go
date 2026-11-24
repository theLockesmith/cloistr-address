package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/btcpay"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// Storage top-ups (storage_topup_10/50/100) can be invoiced through
// POST /api/v1/purchase/product, but a screen selling them had no way to show
// what is on sale or to learn that a payment landed: there was no catalog
// route, and the status route answered 404 for anything but an address.

func TestStorageTopupsRouteIsRegistered(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	for _, r := range h.Router().Routes() {
		if r.Method == http.MethodGet && r.Path == "/api/v1/products/storage" {
			return
		}
	}
	t.Fatal("GET /api/v1/products/storage is not registered")
}

func TestListStorageTopups(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery("FROM products").WillReturnRows(
		sqlmock.NewRows([]string{"id", "display_name", "description", "price_sats", "storage_bytes"}).
			AddRow("storage_topup_10", "Storage +10 GiB (30d)", "10 GiB added to your shared pool for 30 days", int64(500), int64(10737418240)).
			AddRow("storage_topup_50", "Storage +50 GiB (30d)", "50 GiB added to your shared pool for 30 days", int64(2250), int64(53687091200)))

	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{}, store: storage.NewWithDB(db)}
	w := httptest.NewRecorder()
	h.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/products/storage", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var got struct {
		Products []struct {
			ID           string `json:"id"`
			DisplayName  string `json:"display_name"`
			PriceSats    int64  `json:"price_sats"`
			StorageBytes int64  `json:"storage_bytes"`
		} `json:"products"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Products) != 2 {
		t.Fatalf("products = %d, want 2", len(got.Products))
	}
	first := got.Products[0]
	if first.ID != "storage_topup_10" || first.PriceSats != 500 || first.StorageBytes != 10737418240 || first.DisplayName == "" {
		t.Fatalf("first product = %+v", first)
	}
}

func productInvoice(status string) btcpay.Invoice {
	return btcpay.Invoice{ID: "INV777", Status: status, Metadata: map[string]any{
		MetaKind: KindProduct, MetaProductID: "storage_topup_10", MetaPubkey: buyerPubkey,
	}}
}

func TestGetPurchaseStatus_Product(t *testing.T) {
	cases := []struct {
		name       string
		invoice    btcpay.Invoice
		caller     string
		granted    *bool // consulted only for Settled
		wantCode   int
		wantStatus string
	}{
		{"unpaid", productInvoice(btcpay.StatusNew), buyerPubkey, nil, http.StatusOK, PurchaseStatusPending},
		{"settled and granted", productInvoice(btcpay.StatusSettled), buyerPubkey, ptr(true), http.StatusOK, PurchaseStatusCompleted},
		{"settled, webhook has not granted it yet", productInvoice(btcpay.StatusSettled), buyerPubkey, ptr(false), http.StatusOK, PurchaseStatusProcessing},
		{"expired", productInvoice(btcpay.StatusExpired), buyerPubkey, nil, http.StatusOK, PurchaseStatusExpired},
		{"another pubkey's invoice", productInvoice(btcpay.StatusSettled), otherPubkey, nil, http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if tc.granted != nil {
				mock.ExpectQuery("FROM quota_grants").WithArgs("INV777").
					WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(*tc.granted))
			}
			h := &Handler{cfg: &config.Config{Domain: "cloistr.xyz"}, store: storage.NewWithDB(db), btcpay: fakeBTCPay(t, tc.invoice)}

			code, resp, productID := pollProductStatus(t, h, tc.caller, "INV777")
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d", code, tc.wantCode)
			}
			if resp.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", resp.Status, tc.wantStatus)
			}
			if tc.wantCode == http.StatusOK && productID != "storage_topup_10" {
				t.Fatalf("product_id = %q, want storage_topup_10", productID)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func ptr(b bool) *bool { return &b }

// pollProductStatus is pollStatus plus the product_id field, read from the raw
// JSON so this file compiles against a response type that lacks it.
func pollProductStatus(t *testing.T, h *Handler, caller, invoiceID string) (int, PurchaseStatusResponse, string) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/purchase/status/"+invoiceID, nil)
	c.Params = gin.Params{{Key: "invoiceId", Value: invoiceID}}
	c.Set(auth.PubkeyContextKey, caller)
	h.getPurchaseStatus(c)
	var resp PurchaseStatusResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	var raw struct {
		ProductID string `json:"product_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	return w.Code, resp, raw.ProductID
}
