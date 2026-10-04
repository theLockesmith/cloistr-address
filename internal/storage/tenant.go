package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Tenant represents a quota-pooling group.
type Tenant struct {
	ID          string    `json:"id"`
	OwnerPubkey string    `json:"owner_pubkey"`
	CreatedAt   time.Time `json:"created_at"`
}

// TenantMember represents a pubkey's membership in a tenant.
type TenantMember struct {
	TenantID string    `json:"tenant_id"`
	Pubkey   string    `json:"pubkey"`
	JoinedAt time.Time `json:"joined_at"`
}

// CreateTenant inserts a new tenant. Also ensures the owner has a users row:
// production's tenants.owner_pubkey references users(pubkey), so a fresh owner
// key would otherwise fail the foreign key.
func (s *Storage) CreateTenant(ctx context.Context, id, ownerPubkey string) (*Tenant, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO users (pubkey) VALUES ($1)
		ON CONFLICT (pubkey) DO NOTHING
	`, ownerPubkey)
	if err != nil {
		return nil, fmt.Errorf("ensure user for tenant owner: %w", err)
	}

	// display_name is NOT NULL with no default in production; the id is the
	// only name we have at creation time.
	t := &Tenant{ID: id, OwnerPubkey: ownerPubkey}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO tenants (id, display_name, owner_pubkey) VALUES ($1, $1, $2)
		RETURNING created_at
	`, id, ownerPubkey).Scan(&t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create tenant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tenant: %w", err)
	}
	return t, nil
}

// GetTenant retrieves a tenant by id. Returns nil, nil if not found.
func (s *Storage) GetTenant(ctx context.Context, id string) (*Tenant, error) {
	t := &Tenant{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, owner_pubkey, created_at FROM tenants WHERE id = $1
	`, id).Scan(&t.ID, &t.OwnerPubkey, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get tenant: %w", err)
	}
	return t, nil
}

// AddTenantMember adds a pubkey to a tenant. Idempotent: re-adding an existing
// member is a no-op (ON CONFLICT DO NOTHING). Also ensures the pubkey has a
// users row so has_service_access() passes.
func (s *Storage) AddTenantMember(ctx context.Context, tenantID, pubkey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Ensure users row exists (foreign key requirement and service access).
	_, err = tx.ExecContext(ctx, `
		INSERT INTO users (pubkey) VALUES ($1)
		ON CONFLICT (pubkey) DO NOTHING
	`, pubkey)
	if err != nil {
		return fmt.Errorf("ensure user for tenant member: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO tenant_members (tenant_id, pubkey, role, joined_at) VALUES ($1, $2, 'member', NOW())
		ON CONFLICT (tenant_id, pubkey) DO NOTHING
	`, tenantID, pubkey)
	if err != nil {
		return fmt.Errorf("add tenant member: %w", err)
	}

	return tx.Commit()
}

// RemoveTenantMember removes a pubkey from a tenant. No-op if not a member.
func (s *Storage) RemoveTenantMember(ctx context.Context, tenantID, pubkey string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM tenant_members WHERE tenant_id = $1 AND pubkey = $2
	`, tenantID, pubkey)
	if err != nil {
		return fmt.Errorf("remove tenant member: %w", err)
	}
	return nil
}

// ListTenantMembers returns all members of a tenant.
func (s *Storage) ListTenantMembers(ctx context.Context, tenantID string) ([]TenantMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tenant_id, pubkey, joined_at FROM tenant_members
		WHERE tenant_id = $1 ORDER BY joined_at
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list tenant members: %w", err)
	}
	defer rows.Close()

	var members []TenantMember
	for rows.Next() {
		var m TenantMember
		if err := rows.Scan(&m.TenantID, &m.Pubkey, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan tenant member: %w", err)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// SetTenantQuota sets or updates the quota limit for a tenant + quota type.
func (s *Storage) SetTenantQuota(ctx context.Context, tenantID, quotaType string, limit int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tenant_quotas (tenant_id, quota_type_id, quota_limit)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, quota_type_id) DO UPDATE
		SET quota_limit = EXCLUDED.quota_limit
	`, tenantID, quotaType, limit)
	if err != nil {
		return fmt.Errorf("set tenant quota: %w", err)
	}
	return nil
}

// GetTenantForPubkey returns the tenant a pubkey belongs to, or nil if none.
func (s *Storage) GetTenantForPubkey(ctx context.Context, pubkey string) (*Tenant, error) {
	t := &Tenant{}
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.owner_pubkey, t.created_at
		FROM tenants t
		JOIN tenant_members tm ON tm.tenant_id = t.id
		WHERE tm.pubkey = $1
		LIMIT 1
	`, pubkey).Scan(&t.ID, &t.OwnerPubkey, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get tenant for pubkey: %w", err)
	}
	return t, nil
}
