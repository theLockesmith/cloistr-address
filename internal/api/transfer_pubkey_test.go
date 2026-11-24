package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// POST /api/v1/addresses/transfer checked only that new_pubkey was 64
// characters, so 64 non-hex characters, or a key in uppercase hex, were
// accepted and the address went to a "key" no signer will ever produce. These
// requests must be refused before any database work: sqlmock has no
// expectations, so any query fails the request with a 500 instead.

func postTransfer(t *testing.T, caller, newPubkey string) *httptest.ResponseRecorder {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{Domain: "cloistr.xyz"}, store: storage.NewWithDB(db)}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/addresses/transfer",
		bytes.NewBufferString(`{"new_pubkey":"`+newPubkey+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(auth.PubkeyContextKey, caller)
	h.transferAddress(c)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestTransfer_RejectsNonHexPubkey(t *testing.T) {
	w := postTransfer(t, buyerPubkey, strings.Repeat("z", 64))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for 64 non-hex characters; body = %s", w.Code, w.Body.String())
	}
}

func TestTransfer_UppercaseOfOwnKeyIsSelf(t *testing.T) {
	const me = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	w := postTransfer(t, me, strings.ToUpper(me))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "yourself") {
		t.Fatalf("status = %d body = %s, want 400 'Cannot transfer to yourself' (hex is case-insensitive)", w.Code, w.Body.String())
	}
}
