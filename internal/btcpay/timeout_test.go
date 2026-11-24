package btcpay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

/*
In production a product invoice failed with "btcpay create invoice: failed to
send request: ... context deadline exceeded" while the Lightning node was still
starting, and the buyer got a 500 "Failed to create invoice". That is the same
transient trouble as the "lightning node did not reply" 400 and should read the
same way. The caller can only tell them apart from other failures if the client
says which kind of failure it was.
*/

func TestCreateInvoice_TimeoutIsReportedAsATimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	_, err := testClient(t, srv.URL).CreateInvoice(context.Background(), 1000, nil)
	if !errors.Is(err, ErrInvoiceTimeout) {
		t.Fatalf("err = %v; want it to match ErrInvoiceTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; the underlying deadline error should stay in the chain", err)
	}
}

// Other BTCPay failures keep their meaning: a refused connection or a real
// rejection is not a timeout.
func TestCreateInvoice_OtherFailuresAreNotTimeouts(t *testing.T) {
	refused := httptest.NewServer(http.NotFoundHandler())
	url := refused.URL
	refused.Close()
	if _, err := testClient(t, url).CreateInvoice(context.Background(), 1000, nil); err == nil || errors.Is(err, ErrInvoiceTimeout) {
		t.Errorf("connection refused: err = %v; want an error that is not ErrInvoiceTimeout", err)
	}

	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"generic-error","message":"bad amount"}`, http.StatusBadRequest)
	}))
	defer rejecting.Close()
	_, err := testClient(t, rejecting.URL).CreateInvoice(context.Background(), 1000, nil)
	if err == nil || errors.Is(err, ErrInvoiceTimeout) || errors.Is(err, ErrPaymentMethodUnavailable) {
		t.Errorf("real rejection: err = %v; want a plain error", err)
	}
}
