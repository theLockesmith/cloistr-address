package storage

import (
	"context"
	"sync"
	"testing"
)

// Real-Postgres tests for AddInvoiceCreditOnce (migration 012's unique index).
// The webhook now answers 5xx when a race-loss credit fails so BTCPay
// redelivers; these prove a redelivery, even a concurrent one, credits once.

func creditBalance(t *testing.T, s *Storage, pubkey string) int64 {
	t.Helper()
	bal, err := s.GetCredits(context.Background(), pubkey)
	if err != nil {
		t.Fatalf("GetCredits: %v", err)
	}
	return bal
}

func cleanupCredits(t *testing.T, s *Storage, pubkey string) {
	t.Helper()
	_, _ = s.db.Exec(`DELETE FROM credit_history WHERE pubkey = $1`, pubkey)
	_, _ = s.db.Exec(`DELETE FROM pubkey_credits WHERE pubkey = $1`, pubkey)
}

func TestAddInvoiceCreditOnce_RedeliveryCreditsOnce(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	invoice := "inv-" + pk[:12]
	defer cleanupCredits(t, s, pk)

	credited, err := s.AddInvoiceCreditOnce(ctx, pk, 1000, CreditReasonRaceLoss, invoice)
	if err != nil || !credited {
		t.Fatalf("first delivery = (%v, %v), want (true, nil)", credited, err)
	}
	credited, err = s.AddInvoiceCreditOnce(ctx, pk, 1000, CreditReasonRaceLoss, invoice)
	if err != nil || credited {
		t.Fatalf("redelivery = (%v, %v), want (false, nil)", credited, err)
	}
	// A different reason for the same invoice is still the same invoice.
	credited, err = s.AddInvoiceCreditOnce(ctx, pk, 1000, CreditReasonRegistrationError, invoice)
	if err != nil || credited {
		t.Fatalf("same invoice, other reason = (%v, %v), want (false, nil)", credited, err)
	}
	if got := creditBalance(t, s, pk); got != 1000 {
		t.Fatalf("balance = %d, want 1000 (credited once)", got)
	}

	// Another invoice is another credit.
	if credited, err := s.AddInvoiceCreditOnce(ctx, pk, 500, CreditReasonRaceLoss, invoice+"-2"); err != nil || !credited {
		t.Fatalf("second invoice = (%v, %v), want (true, nil)", credited, err)
	}
	if got := creditBalance(t, s, pk); got != 1500 {
		t.Fatalf("balance = %d, want 1500", got)
	}
}

func TestAddInvoiceCreditOnce_ConcurrentDeliveriesCreditOnce(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()
	pk := randPubkey(t)
	invoice := "inv-" + pk[:12]
	defer cleanupCredits(t, s, pk)

	const n = 8
	var wg sync.WaitGroup
	results := make(chan bool, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			credited, err := s.AddInvoiceCreditOnce(ctx, pk, 1000, CreditReasonRaceLoss, invoice)
			if err != nil {
				errs <- err
				return
			}
			results <- credited
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent delivery: %v", err)
	}
	wins := 0
	for c := range results {
		if c {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d of %d concurrent deliveries credited, want exactly 1", wins, n)
	}
	if got := creditBalance(t, s, pk); got != 1000 {
		t.Fatalf("balance = %d, want 1000", got)
	}
}
