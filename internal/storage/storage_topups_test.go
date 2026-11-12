package storage

import (
	"context"
	"testing"
)

// ListStorageTopups against real Postgres: only enabled one-time products that
// grant storage_bytes are on sale, cheapest first.
func TestListStorageTopups(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO products (id, display_name, description, price_sats, product_type, billing_period, grants_quota_increases, enabled) VALUES
		  ('t_topup_big',   'Big',   'big one',   900, 'one_time', NULL, '{"storage_bytes": 300}', TRUE),
		  ('t_topup_small', 'Small', NULL,        100, 'one_time', NULL, '{"storage_bytes": 100}', TRUE),
		  ('t_topup_off',   'Off',   'withdrawn',  50, 'one_time', NULL, '{"storage_bytes": 999}', FALSE),
		  ('t_not_storage', 'Name',  'an address', 10, 'one_time', NULL, '{}',                     TRUE),
		  ('t_monthly',     'Sub',   'monthly',    20, 'subscription', 'monthly', '{"storage_bytes": 5}', TRUE)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed products: %v", err)
	}
	t.Cleanup(func() { _, _ = s.db.ExecContext(ctx, `DELETE FROM products WHERE id LIKE 't\_%'`) })

	got, err := s.ListStorageTopups(ctx)
	if err != nil {
		t.Fatalf("ListStorageTopups: %v", err)
	}
	var ids []string
	for _, p := range got {
		if len(p.ID) > 2 && p.ID[:2] == "t_" {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) != 2 || ids[0] != "t_topup_small" || ids[1] != "t_topup_big" {
		t.Fatalf("on sale = %v, want [t_topup_small t_topup_big]", ids)
	}
	for _, p := range got {
		if p.ID == "t_topup_big" && (p.StorageBytes != 300 || p.Description != "big one" || p.PriceSats != 900) {
			t.Fatalf("t_topup_big = %+v", p)
		}
	}
}
