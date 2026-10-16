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

// Migration 012's index is what the ON CONFLICT clause names. The migration
// runner does not stop on a failed statement, so the index could be absent in
// a deployment; this proves that fails closed: an error, and neither a history
// row nor a balance change (no silent credit that a redelivery could repeat).
func TestAddInvoiceCreditOnce_WithoutIndexFailsClosed(t *testing.T) {
	base, done := testStore(t)
	defer done()
	ctx := context.Background()

	schema := "noidx_" + randPubkey(t)[:12]
	conn, err := base.db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	setup := []string{
		`CREATE SCHEMA ` + schema,
		// credit_history WITHOUT its indexes, so no credit_history_invoice_credit_once.
		`CREATE TABLE ` + schema + `.credit_history (LIKE public.credit_history INCLUDING DEFAULTS INCLUDING CONSTRAINTS)`,
		`CREATE TABLE ` + schema + `.pubkey_credits (LIKE public.pubkey_credits INCLUDING ALL)`,
		`SET search_path TO ` + schema,
	}
	for _, q := range setup {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	defer func() { _, _ = base.db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) }()

	// Pin the Storage to that one connection so every statement sees the schema.
	db := base.db
	db.SetMaxOpenConns(1)
	_ = conn.Close() // return the connection, search_path set, to the pool of one
	s := &Storage{db: db}

	pk := randPubkey(t)
	credited, err := s.AddInvoiceCreditOnce(ctx, pk, 1000, CreditReasonRaceLoss, "inv-noidx")
	if err == nil || credited {
		t.Fatalf("without the index = (%v, %v), want an error and no credit", credited, err)
	}
	var history, balances int
	if err := db.QueryRow(`SELECT count(*) FROM ` + schema + `.credit_history`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM ` + schema + `.pubkey_credits`).Scan(&balances); err != nil {
		t.Fatal(err)
	}
	if history != 0 || balances != 0 {
		t.Fatalf("failed credit left %d history and %d balance rows, want 0 and 0", history, balances)
	}
}
