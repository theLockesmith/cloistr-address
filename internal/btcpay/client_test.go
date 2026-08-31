package btcpay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
)

/*
A second-name purchase failed in production two ways, and both were invisible
from the code:

  1. BTCPay answered HTTP 400 "the lightning node did not reply in a timely
     manner" on roughly one create in six. One transient backend hiccup killed
     the whole purchase.
  2. When BTCPay was merely SLOW, the old 30s http.Client outlasted the edge
     proxy. The service logged a healthy 201 with a BOLT11 after 24.5s and the
     user got an opaque 502. The invoice existed; nobody could pay it.

These tests pin the rules that came out of that: retry the transient 400 (it
creates nothing, so a retry cannot duplicate), never retry anything ambiguous,
and always finish inside the budget.
*/

// The verbatim body captured from the production store, trimmed. Matching on a
// paraphrase would let the real message drift past the detector unnoticed.
const realLNTimeoutBody = `{"code":"generic-error","message":"Error retrieving a matching payment method or rate.\n` +
	`08/27/2026 09:57:41:Info Creation of invoice starting\n` +
	`08/27/2026 09:57:46:Error BTC-LN: NodeInfo failed to be fetched: The lightning node did not reply in a timely manner\n` +
	`08/27/2026 09:57:46:Error BTC-LN: Payment method unavailable (The lightning node did not reply in a timely manner)\n"}`

func testClient(t *testing.T, url string) *Client {
	t.Helper()
	c := NewClient(config.BTCPayConfig{URL: url, APIKey: "k", StoreID: "s"})
	// Shrink the real budget so these run in milliseconds. The RATIOS are what
	// matter to the logic, not the wall-clock values.
	c.budget = 300 * time.Millisecond
	c.attemptTimeout = 100 * time.Millisecond
	c.retryWait = 5 * time.Millisecond
	return c
}

func TestIsTransientLNFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"the real production body", http.StatusBadRequest, realLNTimeoutBody, true},
		{"payment method unavailable", http.StatusBadRequest, "BTC-LN: Payment method unavailable (x)", true},
		// A 400 we do not recognise is a real rejection. Retrying it would
		// replay an invalid request, and might replay one that DID create an
		// invoice.
		{"unrelated 400 is not transient", http.StatusBadRequest, `{"code":"invalid-amount"}`, false},
		{"same text on a 500 is not our case", http.StatusInternalServerError, realLNTimeoutBody, false},
		{"success is not a failure", http.StatusOK, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientLNFailure(tc.status, tc.body); got != tc.want {
				t.Errorf("isTransientLNFailure(%d, %.40q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// THE BUG. One transient Lightning timeout used to fail the entire purchase.
func TestCreateInvoice_RetriesTransientLNFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, realLNTimeoutBody)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"inv-2","storeId":"s","amount":"1000","status":"New"}`)
	}))
	defer srv.Close()

	inv, err := testClient(t, srv.URL).CreateInvoice(context.Background(), 1000, nil)
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	if inv.ID != "inv-2" {
		t.Errorf("invoice id = %q, want inv-2", inv.ID)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("attempts = %d, want 2 (one failure then one success)", n)
	}
}

// A non-transient error must cost exactly one attempt. Retrying a real
// rejection wastes the budget and can duplicate work.
func TestCreateInvoice_DoesNotRetryRealRejection(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":"unauthorized"}`)
	}))
	defer srv.Close()

	_, err := testClient(t, srv.URL).CreateInvoice(context.Background(), 1000, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrPaymentMethodUnavailable) {
		t.Error("a 401 was classified as a transient Lightning failure")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("attempts = %d, want exactly 1", n)
	}
}

// When Lightning never recovers the caller must be able to TELL that is what
// happened, so the API can answer 503 "try again" instead of a blank 500.
func TestCreateInvoice_ReportsPaymentMethodUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, realLNTimeoutBody)
	}))
	defer srv.Close()

	_, err := testClient(t, srv.URL).CreateInvoice(context.Background(), 1000, nil)
	if !errors.Is(err, ErrPaymentMethodUnavailable) {
		t.Fatalf("err = %v, want ErrPaymentMethodUnavailable", err)
	}
}

// A hung BTCPay must NOT be retried: the abandoned request may already have
// created an invoice, and a second one would leave the user two to choose from.
// It must also return inside the budget rather than outliving the edge proxy.
func TestCreateInvoice_HangIsBoundedAndNotRetried(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	c := testClient(t, srv.URL)
	start := time.Now()
	_, err := c.CreateInvoice(context.Background(), 1000, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a hung server")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("attempts = %d, want 1: a hang must not be retried", n)
	}
	// The point of the whole change: bounded, and bounded by OUR budget.
	if elapsed > c.budget+(200*time.Millisecond) {
		t.Errorf("took %v, want <= budget %v — this is the 502 bug", elapsed, c.budget)
	}
}

// A fast-failing BTCPay must not be hammered. Before the explicit cap, a
// time-only guard fitted 30 attempts into a 200ms budget — a retry storm aimed
// at the Lightning node we are already waiting on.
func TestCreateInvoice_CapsAttemptsWhenFailuresAreFast(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, realLNTimeoutBody)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	// Budget deliberately far larger than the failures take, so only the cap
	// can stop the loop.
	c.budget = 5 * time.Second
	c.retryWait = time.Millisecond

	_, err := c.CreateInvoice(context.Background(), 1000, nil)
	if !errors.Is(err, ErrPaymentMethodUnavailable) {
		t.Fatalf("err = %v, want ErrPaymentMethodUnavailable", err)
	}
	if n := atomic.LoadInt32(&calls); n != maxInvoiceAttempts {
		t.Errorf("attempts = %d, want the %d cap", n, maxInvoiceAttempts)
	}
}

// The budget covers the retries too, not just one attempt.
func TestCreateInvoice_StopsWithinBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, realLNTimeoutBody)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	start := time.Now()
	_, _ = c.CreateInvoice(context.Background(), 1000, nil)
	if elapsed := time.Since(start); elapsed > c.budget+(200*time.Millisecond) {
		t.Errorf("retry loop ran %v, past budget %v", elapsed, c.budget)
	}
}

// A caller that goes away (client disconnect) must stop the work immediately
// rather than holding a BTCPay connection for the full budget.
func TestCreateInvoice_HonoursCallerCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := testClient(t, srv.URL).CreateInvoice(ctx, 1000, nil)
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 80*time.Millisecond {
		t.Errorf("took %v after a 20ms cancel — caller cancellation ignored", elapsed)
	}
}
