package api

import (
	"context"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

func settleHandler(t *testing.T) (*Handler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Handler{cfg: &config.Config{}, store: storage.NewWithDB(db)}, mock
}

func TestSettlementKind(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		want string
	}{
		{"explicit product", map[string]any{MetaKind: KindProduct, MetaProductID: "storage_topup_10"}, KindProduct},
		{"explicit address", map[string]any{MetaKind: KindAddress, MetaUsername: "coldforge"}, KindAddress},
		{"product inferred from product_id", map[string]any{MetaProductID: "storage_topup_10"}, KindProduct},
		// Invoices minted before `kind` existed carry only a username and BTCPay
		// will still deliver them. Failing to infer here drops a real payment.
		{"legacy invoice: bare username", map[string]any{MetaUsername: "coldforge"}, KindAddress},
		{"nothing purchasable", map[string]any{"pubkey": "abc"}, ""},
		{"empty strings are not a kind", map[string]any{MetaUsername: "", MetaProductID: ""}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := settlementKind(tc.meta); got != tc.want {
				t.Fatalf("settlementKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGrantWindow(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	if got, err := grantWindow(now, ""); err != nil || got != nil {
		t.Fatalf("empty period = (%v, %v), want (nil, nil) — a permanent grant", got, err)
	}

	got, err := grantWindow(now, "30d")
	if err != nil {
		t.Fatalf("30d: %v", err)
	}
	if want := now.AddDate(0, 0, 30); !got.Equal(want) {
		t.Fatalf("30d = %v, want %v", got, want)
	}

	// An unparseable period must NOT fall through to "permanent". Granting
	// 100 GiB forever because someone typed "30 days" is the expensive way to
	// be wrong.
	for _, bad := range []string{"30 days", "monthly", "0d", "-5d", "d"} {
		if _, err := grantWindow(now, bad); err == nil {
			t.Errorf("billing_period %q was accepted; it must error, never mean permanent", bad)
		}
	}
}

func TestSettleProduct_GrantsQuotaWithExpiry(t *testing.T) {
	h, mock := settleHandler(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery("EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM products").WillReturnRows(
		sqlmock.NewRows([]string{"id", "display_name", "price_sats", "product_type", "billing_period", "grants_quota_increases", "enabled"}).
			AddRow("storage_topup_10", "Storage +10 GiB (30d)", int64(500), "one_time", "30d", []byte(`{"storage_bytes":10737418240}`), true))
	mock.ExpectExec("INSERT INTO quota_grants").
		WithArgs("pk", "storage_bytes", int64(10737418240), "purchase", "inv1", now.AddDate(0, 0, 30)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	out, err := h.settleProduct(context.Background(), "inv1", "pk", "storage_topup_10", now)
	if err != nil {
		t.Fatalf("settleProduct: %v", err)
	}
	if out.Status != "granted" {
		t.Fatalf("status = %q, want granted", out.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSettleProduct_IsIdempotentOnInvoiceID(t *testing.T) {
	// BTCPay retries webhooks. Without this the retry doubles the storage.
	h, mock := settleHandler(t)
	mock.ExpectQuery("EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	out, err := h.settleProduct(context.Background(), "inv1", "pk", "storage_topup_10", time.Now())
	if err != nil {
		t.Fatalf("settleProduct: %v", err)
	}
	if out.Status != "already_applied" {
		t.Fatalf("status = %q, want already_applied", out.Status)
	}
	// Crucially: no product read and no INSERT happened.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a retry must not touch the catalog or grant again: %v", err)
	}
}

func TestSettleProduct_UnknownProductIsAnError(t *testing.T) {
	// The user paid. Swallowing this would take their sats and grant nothing.
	h, mock := settleHandler(t)
	mock.ExpectQuery("EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM products").WillReturnError(errNoRowsForTest())

	_, err := h.settleProduct(context.Background(), "inv1", "pk", "gone_product", time.Now())
	if err == nil {
		t.Fatal("a settled invoice for an unknown product must error, not succeed quietly")
	}
	if !strings.Contains(err.Error(), "gone_product") {
		t.Fatalf("error must name the product for reconciliation, got: %v", err)
	}
}

func TestSettleProduct_ProductGrantingNothingIsAnError(t *testing.T) {
	h, mock := settleHandler(t)
	mock.ExpectQuery("EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM products").WillReturnRows(
		sqlmock.NewRows([]string{"id", "display_name", "price_sats", "product_type", "billing_period", "grants_quota_increases", "enabled"}).
			AddRow("mystery", "Mystery", int64(500), "one_time", "", []byte(`{}`), true))

	if _, err := h.settleProduct(context.Background(), "inv1", "pk", "mystery", time.Now()); err == nil {
		t.Fatal("a paid product that grants nothing must error rather than report success")
	}
}
