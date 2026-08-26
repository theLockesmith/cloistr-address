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

	// No window key at all: a permanent grant.
	if got, err := grantWindow(now, map[string]int64{"storage_bytes": 1}); err != nil || got != nil {
		t.Fatalf("no window = (%v, %v), want (nil, nil) — a permanent grant", got, err)
	}

	got, err := grantWindow(now, map[string]int64{"storage_bytes": 1, QuotaExpiresDaysKey: 30})
	if err != nil {
		t.Fatalf("30 days: %v", err)
	}
	if want := now.AddDate(0, 0, 30); !got.Equal(want) {
		t.Fatalf("30 days = %v, want %v", got, want)
	}

	// A nonsense window must NOT fall through to "permanent". Granting 100 GiB
	// forever because someone typed a zero is the expensive way to be wrong.
	for _, bad := range []int64{0, -5} {
		if _, err := grantWindow(now, map[string]int64{QuotaExpiresDaysKey: bad}); err == nil {
			t.Errorf("%s=%d was accepted; it must error, never mean permanent", QuotaExpiresDaysKey, bad)
		}
	}
}

func TestSettleProduct_GrantsQuotaWithExpiry(t *testing.T) {
	h, mock := settleHandler(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery("EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM products").WillReturnRows(
		sqlmock.NewRows([]string{"id", "display_name", "price_sats", "product_type", "billing_period", "grants_quota_increases", "enabled"}).
			AddRow("storage_topup_10", "Storage +10 GiB (30d)", int64(500), "one_time", "", []byte(`{"storage_bytes":10737418240,"expires_days":30}`), true))
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

func TestSettleProduct_DoesNotGrantTheWindowAsQuota(t *testing.T) {
	// expires_days lives in the same jsonb as the real quotas, so a naive loop
	// grants the user 30 bytes of a quota type called "expires_days".
	h, mock := settleHandler(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery("EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM products").WillReturnRows(
		sqlmock.NewRows([]string{"id", "display_name", "price_sats", "product_type", "billing_period", "grants_quota_increases", "enabled"}).
			AddRow("storage_topup_10", "Storage +10 GiB (30d)", int64(500), "one_time", "",
				[]byte(`{"storage_bytes":10737418240,"expires_days":30}`), true))
	// EXACTLY ONE grant, for storage_bytes, with the 30-day expiry applied.
	mock.ExpectExec("INSERT INTO quota_grants").
		WithArgs("pk", "storage_bytes", int64(10737418240), "purchase", "inv1", now.AddDate(0, 0, 30)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	out, err := h.settleProduct(context.Background(), "inv1", "pk", "storage_topup_10", now)
	if err != nil {
		t.Fatalf("settleProduct: %v", err)
	}
	if _, leaked := out.Detail["granted"].(map[string]any)[QuotaExpiresDaysKey]; leaked {
		t.Fatalf("%s was reported as a granted quota: %+v", QuotaExpiresDaysKey, out.Detail)
	}
	// sqlmock fails the test if a second INSERT was attempted.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/extra expectations — the window was granted as a quota: %v", err)
	}
}

func TestSettleProduct_WindowOnlyPayloadIsAnError(t *testing.T) {
	// A product whose payload is nothing but a window grants nothing at all.
	h, mock := settleHandler(t)
	mock.ExpectQuery("EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM products").WillReturnRows(
		sqlmock.NewRows([]string{"id", "display_name", "price_sats", "product_type", "billing_period", "grants_quota_increases", "enabled"}).
			AddRow("broken", "Broken", int64(500), "one_time", "", []byte(`{"expires_days":30}`), true))

	if _, err := h.settleProduct(context.Background(), "inv1", "pk", "broken", time.Now()); err == nil {
		t.Fatal("a paid product granting only a window must error, not report success")
	}
}
