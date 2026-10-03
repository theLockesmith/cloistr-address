package storage

import (
	"context"
	"testing"
)

// DB-integration tests for tenant quota pooling. They run only when
// TEST_DATABASE_URL points at a Postgres with migration 010 applied.
//
//	TEST_DATABASE_URL='postgres://...' go test ./internal/storage/ -run Tenant -v

func cleanupTenant(t *testing.T, s *Storage, tenantID string, pubkeys ...string) {
	t.Helper()
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenantID)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM tenant_members WHERE tenant_id = $1`, tenantID)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	for _, pk := range pubkeys {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM user_quota_usage WHERE pubkey = $1`, pk)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM users WHERE pubkey = $1`, pk)
	}
}

func TestTenantQuotaPooling(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()

	ownerPK := randPubkey(t)
	memberPK := randPubkey(t)
	tenantID := "test-tenant-" + ownerPK[:8]
	defer cleanupTenant(t, s, tenantID, ownerPK, memberPK)

	// Create tenant and set a pooled quota of 1 GB for storage_bytes.
	_, err := s.CreateTenant(ctx, tenantID, ownerPK)
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.SetTenantQuota(ctx, tenantID, "storage_bytes", 1073741824); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}

	// Add the member.
	if err := s.AddTenantMember(ctx, tenantID, memberPK); err != nil {
		t.Fatalf("AddTenantMember: %v", err)
	}

	// Member's quota should now come from the tenant pool (1 GB limit).
	q, err := s.EffectiveQuota(ctx, memberPK, "storage_bytes")
	if err != nil {
		t.Fatalf("EffectiveQuota (in tenant): %v", err)
	}
	if q.Limit != 1073741824 {
		t.Fatalf("in-tenant limit = %d, want 1073741824", q.Limit)
	}

	// Record some usage so we can see the number change after removal.
	if err := s.RecordServiceUsage(ctx, memberPK, "storage_bytes", "blossom", 50000); err != nil {
		t.Fatalf("RecordServiceUsage: %v", err)
	}

	q2, err := s.EffectiveQuota(ctx, memberPK, "storage_bytes")
	if err != nil {
		t.Fatalf("EffectiveQuota (with usage): %v", err)
	}
	if q2.Used != 50000 {
		t.Fatalf("in-tenant used = %d, want 50000", q2.Used)
	}
	if q2.Remaining != 1073741824-50000 {
		t.Fatalf("in-tenant remaining = %d, want %d", q2.Remaining, 1073741824-50000)
	}

	// Remove the member from the tenant.
	if err := s.RemoveTenantMember(ctx, tenantID, memberPK); err != nil {
		t.Fatalf("RemoveTenantMember: %v", err)
	}

	// After removal, the member's quota should fall back to their individual
	// allocation, NOT the tenant pool. For a pubkey with no named address and
	// no per-user override, this is the anonymous-tier default from quota_types.
	q3, err := s.EffectiveQuota(ctx, memberPK, "storage_bytes")
	if err != nil {
		t.Fatalf("EffectiveQuota (after removal): %v", err)
	}
	if q3.Limit == 1073741824 {
		t.Fatal("after removal the member still sees the tenant limit; fallback is broken")
	}
	// The individual limit should be the anonymous default (whatever the DB has).
	// The key assertion: it is NOT the tenant limit.
	t.Logf("fallback confirmed: limit went from %d (tenant) to %d (individual), used=%d",
		q.Limit, q3.Limit, q3.Used)
}

func TestTenantListMembersAgainstProdSchema(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()

	ownerPK := randPubkey(t)
	memberPK := randPubkey(t)
	tenantID := "test-list-" + ownerPK[:8]
	defer cleanupTenant(t, s, tenantID, ownerPK, memberPK)

	if _, err := s.CreateTenant(ctx, tenantID, ownerPK); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.AddTenantMember(ctx, tenantID, memberPK); err != nil {
		t.Fatalf("AddTenantMember: %v", err)
	}

	members, err := s.ListTenantMembers(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListTenantMembers: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("got %d members, want 1", len(members))
	}
	if members[0].Pubkey != memberPK {
		t.Fatalf("got pubkey=%q, want %q", members[0].Pubkey, memberPK)
	}
	if members[0].JoinedAt.IsZero() {
		t.Fatal("joined_at should not be zero")
	}
}

func TestTenantQuotaPoolsUsageAcrossMembers(t *testing.T) {
	s, done := testStore(t)
	defer done()
	ctx := context.Background()

	ownerPK := randPubkey(t)
	m1 := randPubkey(t)
	m2 := randPubkey(t)
	tenantID := "test-pool-" + ownerPK[:8]
	defer cleanupTenant(t, s, tenantID, ownerPK, m1, m2)

	_, err := s.CreateTenant(ctx, tenantID, ownerPK)
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.SetTenantQuota(ctx, tenantID, "storage_bytes", 1000000); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	if err := s.AddTenantMember(ctx, tenantID, m1); err != nil {
		t.Fatalf("AddTenantMember m1: %v", err)
	}
	if err := s.AddTenantMember(ctx, tenantID, m2); err != nil {
		t.Fatalf("AddTenantMember m2: %v", err)
	}

	// Each member uses some storage.
	if err := s.RecordServiceUsage(ctx, m1, "storage_bytes", "blossom", 300000); err != nil {
		t.Fatalf("RecordServiceUsage m1: %v", err)
	}
	if err := s.RecordServiceUsage(ctx, m2, "storage_bytes", "blossom", 200000); err != nil {
		t.Fatalf("RecordServiceUsage m2: %v", err)
	}

	// Both members should see the SAME pooled usage (500000 total).
	q1, err := s.EffectiveQuota(ctx, m1, "storage_bytes")
	if err != nil {
		t.Fatalf("EffectiveQuota m1: %v", err)
	}
	q2, err := s.EffectiveQuota(ctx, m2, "storage_bytes")
	if err != nil {
		t.Fatalf("EffectiveQuota m2: %v", err)
	}

	if q1.Used != 500000 {
		t.Errorf("m1 sees used=%d, want 500000 (pooled)", q1.Used)
	}
	if q2.Used != 500000 {
		t.Errorf("m2 sees used=%d, want 500000 (pooled)", q2.Used)
	}
	if q1.Remaining != 500000 {
		t.Errorf("m1 remaining=%d, want 500000", q1.Remaining)
	}
	if q2.Remaining != 500000 {
		t.Errorf("m2 remaining=%d, want 500000", q2.Remaining)
	}
}
