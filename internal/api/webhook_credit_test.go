package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/btcpay"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// A buyer who pays for a name someone else got first is owed a credit. BTCPay
// only redelivers a webhook answered with a non-2xx, so every way that credit
// can fail must be a 5xx, and the redelivery must not credit twice.

const webhookSecret = "whsec"

// webhookBTCPay serves the invoice the webhook looks up, or fails the lookup.
func webhookBTCPay(t *testing.T, lookupFails bool) *btcpay.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lookupFails {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "INV1", "status": "Settled", "amount": "1000"})
	}))
	t.Cleanup(srv.Close)
	return btcpay.NewClient(config.BTCPayConfig{URL: srv.URL, APIKey: "k", StoreID: "store1", WebhookSecret: webhookSecret})
}

func deliverSettled(t *testing.T, h *Handler) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"type": btcpay.EventInvoiceSettled, "invoiceId": "INV1", "storeId": "store1",
		"metadata": map[string]any{MetaKind: KindAddress, MetaUsername: "alice", MetaPubkey: buyerPubkey},
	})
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhook/payment", bytes.NewReader(body))
	c.Request.Header.Set("BTCPay-Sig", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	h.handleBTCPayWebhook(c)

	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp
}

func webhookHandler(t *testing.T, lookupFails bool) (*Handler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Handler{cfg: &config.Config{Domain: "cloistr.xyz"}, store: storage.NewWithDB(db), btcpay: webhookBTCPay(t, lookupFails)}, mock
}

// The name is already someone else's: AtomicRegisterAddress returns nil.
func expectRaceLoss(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectQuery("FROM addresses").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()
}

func TestWebhookRaceLoss_CreditFailureIs5xx(t *testing.T) {
	h, mock := webhookHandler(t, false)
	expectRaceLoss(mock)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO").WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	code, resp := deliverSettled(t, h)
	if code < 500 {
		t.Fatalf("credit failed but webhook answered %d %v; BTCPay will not redeliver and the buyer loses the credit", code, resp)
	}
}

func TestWebhookInvoiceLookupFailureIs5xx(t *testing.T) {
	h, mock := webhookHandler(t, true)
	// The old code carried on with amount 0 into a race loss.
	expectRaceLoss(mock)

	code, resp := deliverSettled(t, h)
	if code < 500 {
		t.Fatalf("invoice lookup failed but webhook answered %d %v; a race loss would credit 0", code, resp)
	}
}

func TestWebhookRaceLoss_RedeliveryDoesNotDoubleCredit(t *testing.T) {
	h, mock := webhookHandler(t, false)

	// First delivery: history row inserted, balance credited.
	expectRaceLoss(mock)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO credit_history").WithArgs(buyerPubkey, int64(1000), storage.CreditReasonRaceLoss, "INV1").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO pubkey_credits").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Redelivery: the history insert conflicts, and the balance is not touched.
	expectRaceLoss(mock)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO credit_history").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	for i, label := range []string{"first delivery", "redelivery"} {
		code, resp := deliverSettled(t, h)
		if code != http.StatusOK || resp["status"] != "race_loss" {
			t.Fatalf("%s (%d) = %d %v, want 200 race_loss", label, i, code, resp)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("credit was not exactly once: %v", err)
	}
}
