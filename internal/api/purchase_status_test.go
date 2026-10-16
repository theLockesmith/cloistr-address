package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/btcpay"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

const (
	buyerPubkey = "1111111111111111111111111111111111111111111111111111111111111111"
	otherPubkey = "2222222222222222222222222222222222222222222222222222222222222222"
)

// The purchase page polls GET /api/v1/purchase/status/:id every few seconds
// and only leaves the QR code when it says "completed". The route was never
// registered, so every poll was a 404 and a paid name never confirmed.
func TestPurchaseStatusRouteIsRegistered(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	for _, r := range h.Router().Routes() {
		if r.Method == http.MethodGet && r.Path == "/api/v1/purchase/status/:invoiceId" {
			return
		}
	}
	t.Fatal("GET /api/v1/purchase/status/:invoiceId is not registered; the purchase page polls it")
}

// fakeBTCPay serves one invoice by id, the way Greenfield does.
func fakeBTCPay(t *testing.T, inv btcpay.Invoice) *btcpay.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/stores/store1/invoices/"+inv.ID {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": inv.ID, "status": inv.Status, "amount": "1000", "metadata": inv.Metadata,
		})
	}))
	t.Cleanup(srv.Close)
	return btcpay.NewClient(config.BTCPayConfig{URL: srv.URL, APIKey: "k", StoreID: "store1"})
}

func addressInvoice(status string) btcpay.Invoice {
	return btcpay.Invoice{ID: "INV123", Status: status, Metadata: map[string]any{
		MetaKind: KindAddress, MetaUsername: "alice", MetaPubkey: buyerPubkey,
	}}
}

func addressRow(pubkey string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{"id", "username", "domain", "pubkey", "active", "verified", "is_primary",
		"nip05_active", "display_name", "created_at", "updated_at", "expires_at", "grace_period_ends", "ban_reason"}).
		AddRow(1, "alice", "cloistr.xyz", pubkey, true, true, true, true, nil, now, now, nil, nil, nil)
}

func pollStatus(t *testing.T, h *Handler, caller, invoiceID string) (int, PurchaseStatusResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/purchase/status/"+invoiceID, nil)
	c.Params = gin.Params{{Key: "invoiceId", Value: invoiceID}}
	c.Set(auth.PubkeyContextKey, caller)
	h.getPurchaseStatus(c)
	var resp PurchaseStatusResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp
}

func TestGetPurchaseStatus(t *testing.T) {
	cases := []struct {
		name       string
		invoice    btcpay.Invoice
		caller     string
		invoiceID  string
		addrOwner  string // "" = no address row; only consulted for Settled
		wantCode   int
		wantStatus string
	}{
		{"unpaid", addressInvoice(btcpay.StatusNew), buyerPubkey, "INV123", "", http.StatusOK, PurchaseStatusPending},
		{"payment seen, not confirmed", addressInvoice(btcpay.StatusProcessing), buyerPubkey, "INV123", "", http.StatusOK, PurchaseStatusProcessing},
		{"settled and registered to the buyer", addressInvoice(btcpay.StatusSettled), buyerPubkey, "INV123", buyerPubkey, http.StatusOK, PurchaseStatusCompleted},
		{"settled, webhook has not registered it yet", addressInvoice(btcpay.StatusSettled), buyerPubkey, "INV123", "", http.StatusOK, PurchaseStatusProcessing},
		// Race loss: the webhook credited this payment, so the page must stop
		// polling rather than wait for a name that went to someone else.
		{"settled, but someone else won the name", addressInvoice(btcpay.StatusSettled), buyerPubkey, "INV123", otherPubkey, http.StatusOK, PurchaseStatusFailed},
		{"expired", addressInvoice(btcpay.StatusExpired), buyerPubkey, "INV123", "", http.StatusOK, PurchaseStatusExpired},
		{"invalid", addressInvoice(btcpay.StatusInvalid), buyerPubkey, "INV123", "", http.StatusOK, PurchaseStatusFailed},
		{"another pubkey's invoice", addressInvoice(btcpay.StatusSettled), otherPubkey, "INV123", "", http.StatusNotFound, ""},
		{"not an address purchase", btcpay.Invoice{ID: "INV123", Status: btcpay.StatusSettled, Metadata: map[string]any{
			MetaKind: KindProduct, MetaProductID: "storage_topup_10", MetaPubkey: buyerPubkey}}, buyerPubkey, "INV123", "", http.StatusNotFound, ""},
		{"unknown invoice", addressInvoice(btcpay.StatusNew), buyerPubkey, "NOPE", "", http.StatusNotFound, ""},
		{"malformed id never reaches BTCPay", addressInvoice(btcpay.StatusNew), buyerPubkey, "..%2Fwebhooks", "", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if tc.wantCode == http.StatusOK && tc.invoice.Status == btcpay.StatusSettled {
				q := mock.ExpectQuery("FROM addresses").WithArgs("alice", "cloistr.xyz")
				if tc.addrOwner == "" {
					q.WillReturnRows(sqlmock.NewRows([]string{"id"}))
				} else {
					q.WillReturnRows(addressRow(tc.addrOwner))
				}
			}
			h := &Handler{cfg: &config.Config{Domain: "cloistr.xyz"}, store: storage.NewWithDB(db), btcpay: fakeBTCPay(t, tc.invoice)}

			code, resp := pollStatus(t, h, tc.caller, tc.invoiceID)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d", code, tc.wantCode)
			}
			if resp.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", resp.Status, tc.wantStatus)
			}
			if tc.wantCode == http.StatusOK && resp.Username != "alice" {
				t.Fatalf("username = %q, want alice", resp.Username)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
