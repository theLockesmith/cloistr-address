package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by admin store methods when the target row is absent.
var ErrNotFound = errors.New("not found")

// AuditEntry is one row written to audit_log for an admin action. The DB
// BEFORE INSERT trigger fills prev_hash/entry_hash to chain the entry.
type AuditEntry struct {
	TableName   string
	RecordID    string
	Action      string
	ActorPubkey string
	OldValues   any    // marshaled to JSONB (nil = NULL)
	NewValues   any    // marshaled to JSONB (nil = NULL)
	Metadata    any    // {source, user_agent, request_id, ...}
	Signature   string // NIP-98 schnorr sig of the admin action (hex)
}

func toJSON(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// writeAudit inserts an audit row within an existing transaction.
func writeAudit(ctx context.Context, tx *sql.Tx, e AuditEntry) error {
	oldV, err := toJSON(e.OldValues)
	if err != nil {
		return fmt.Errorf("marshal old_values: %w", err)
	}
	newV, err := toJSON(e.NewValues)
	if err != nil {
		return fmt.Errorf("marshal new_values: %w", err)
	}
	meta, err := toJSON(e.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_log
			(table_name, record_id, action, actor_pubkey, actor_type,
			 old_values, new_values, metadata, signature)
		VALUES ($1, $2, $3, $4, 'user', $5, $6, $7, $8)
	`, e.TableName, e.RecordID, e.Action, e.ActorPubkey, oldV, newV, meta, e.Signature)
	if err != nil {
		return fmt.Errorf("write audit: %w", err)
	}
	return nil
}

// LogAdminAudit writes a single audit row in its own transaction. Use for
// actions whose primary mutation already has its own bookkeeping (e.g. credits,
// which also records credit_history).
func (s *Storage) LogAdminAudit(ctx context.Context, e AuditEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeAudit(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

// IsPlatformAdmin reports whether pubkey is an enabled platform super-admin.
func (s *Storage) IsPlatformAdmin(ctx context.Context, pubkey string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM users
			WHERE pubkey = $1 AND is_platform_admin = TRUE AND enabled = TRUE
		)
	`, pubkey).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check platform admin: %w", err)
	}
	return ok, nil
}

// IsServiceAdmin reuses the DB function is_service_admin (platform admins pass too).
func (s *Storage) IsServiceAdmin(ctx context.Context, pubkey, serviceID string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT is_service_admin($1, $2)`, pubkey, serviceID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check service admin: %w", err)
	}
	return ok, nil
}

// ensureUser upserts a users row so FKs (addresses.pubkey, user_quotas.pubkey) hold.
func ensureUser(ctx context.Context, tx *sql.Tx, pubkey string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO users (pubkey) VALUES ($1)
		ON CONFLICT (pubkey) DO NOTHING
	`, pubkey)
	if err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Addresses (admin)
// ---------------------------------------------------------------------------

// AdminGrantAddress creates an address for pubkey, bypassing payment and the
// reserved-list. If the pubkey already holds an address it errors (one per user).
func (s *Storage) AdminGrantAddress(ctx context.Context, actor, sig, username, domain, pubkey string, displayName *string) (*Address, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := ensureUser(ctx, tx, pubkey); err != nil {
		return nil, err
	}

	addr := &Address{}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO addresses (username, domain, pubkey, active, verified, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, true, true, $4, NOW(), NOW())
		RETURNING id, username, domain, pubkey, active, verified, display_name, created_at, updated_at
	`, username, domain, pubkey, displayName).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.DisplayName, &addr.CreatedAt, &addr.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert address: %w", err)
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:   "addresses",
		RecordID:    fmt.Sprintf("%d", addr.ID),
		Action:      "address.grant",
		ActorPubkey: actor,
		NewValues:   map[string]any{"username": username, "domain": domain, "pubkey": pubkey, "display_name": displayName},
		Metadata:    map[string]any{"source": "admin"},
		Signature:   sig,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return addr, nil
}

// AdminRevokeAddress deactivates an address (soft) and records a ban reason.
func (s *Storage) AdminRevokeAddress(ctx context.Context, actor, sig, username, domain, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	var prevPubkey string
	err = tx.QueryRowContext(ctx, `
		UPDATE addresses SET active = false, ban_reason = $3, updated_at = NOW()
		WHERE username = $1 AND domain = $2
		RETURNING id, pubkey
	`, username, domain, reason).Scan(&id, &prevPubkey)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke address: %w", err)
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:   "addresses",
		RecordID:    fmt.Sprintf("%d", id),
		Action:      "address.revoke",
		ActorPubkey: actor,
		OldValues:   map[string]any{"active": true, "pubkey": prevPubkey},
		NewValues:   map[string]any{"active": false, "ban_reason": reason},
		Metadata:    map[string]any{"source": "admin"},
		Signature:   sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// AdminTransferAddress moves an address to a new pubkey (admin override).
func (s *Storage) AdminTransferAddress(ctx context.Context, actor, sig, username, domain, toPubkey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := ensureUser(ctx, tx, toPubkey); err != nil {
		return err
	}

	var id int64
	var fromPubkey string
	err = tx.QueryRowContext(ctx, `SELECT id, pubkey FROM addresses WHERE username = $1 AND domain = $2`, username, domain).Scan(&id, &fromPubkey)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup address: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE addresses SET pubkey = $2, last_transfer_at = NOW(), updated_at = NOW() WHERE id = $1
	`, id, toPubkey)
	if err != nil {
		return fmt.Errorf("transfer address: %w", err)
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:   "addresses",
		RecordID:    fmt.Sprintf("%d", id),
		Action:      "address.transfer",
		ActorPubkey: actor,
		OldValues:   map[string]any{"pubkey": fromPubkey},
		NewValues:   map[string]any{"pubkey": toPubkey},
		Metadata:    map[string]any{"source": "admin"},
		Signature:   sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// AdminListAddressesByPubkey returns all addresses (active or not) for a pubkey.
func (s *Storage) AdminListAddressesByPubkey(ctx context.Context, pubkey string) ([]Address, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified, display_name, created_at, updated_at, ban_reason
		FROM addresses WHERE pubkey = $1 ORDER BY created_at
	`, pubkey)
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	defer rows.Close()

	var out []Address
	for rows.Next() {
		var a Address
		if err := rows.Scan(&a.ID, &a.Username, &a.Domain, &a.Pubkey, &a.Active, &a.Verified,
			&a.DisplayName, &a.CreatedAt, &a.UpdatedAt, &a.BanReason); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Reserved usernames
// ---------------------------------------------------------------------------

// ReservedUsername mirrors a reserved_usernames row.
type ReservedUsername struct {
	Username          string
	ReservedForPubkey *string // NULL = blocked, pubkey = held for that user
	Reason            *string
	ReservedAt        time.Time
}

// AddReserved reserves a username. forPubkey nil = block entirely.
func (s *Storage) AddReserved(ctx context.Context, actor, sig, username string, forPubkey *string, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO reserved_usernames (username, reserved_for_pubkey, reason)
		VALUES ($1, $2, $3)
		ON CONFLICT (username) DO UPDATE SET reserved_for_pubkey = $2, reason = $3
	`, username, forPubkey, reason)
	if err != nil {
		return fmt.Errorf("add reserved: %w", err)
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:   "reserved_usernames",
		RecordID:    username,
		Action:      "reserved.add",
		ActorPubkey: actor,
		NewValues:   map[string]any{"reserved_for_pubkey": forPubkey, "reason": reason},
		Metadata:    map[string]any{"source": "admin"},
		Signature:   sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveReserved unreserves a username.
func (s *Storage) RemoveReserved(ctx context.Context, actor, sig, username string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `DELETE FROM reserved_usernames WHERE username = $1`, username)
	if err != nil {
		return fmt.Errorf("remove reserved: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:   "reserved_usernames",
		RecordID:    username,
		Action:      "reserved.remove",
		ActorPubkey: actor,
		Metadata:    map[string]any{"source": "admin"},
		Signature:   sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ListReserved returns all reserved usernames.
func (s *Storage) ListReserved(ctx context.Context) ([]ReservedUsername, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT username, reserved_for_pubkey, reason, reserved_at
		FROM reserved_usernames ORDER BY username
	`)
	if err != nil {
		return nil, fmt.Errorf("list reserved: %w", err)
	}
	defer rows.Close()

	var out []ReservedUsername
	for rows.Next() {
		var r ReservedUsername
		if err := rows.Scan(&r.Username, &r.ReservedForPubkey, &r.Reason, &r.ReservedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Quotas
// ---------------------------------------------------------------------------

// QuotaView is a per-user quota with its type metadata.
type QuotaView struct {
	QuotaTypeID  string
	DisplayName  string
	Unit         string
	QuotaLimit   int64 // 0 = unlimited
	CurrentUsage int64
	IsOverride   bool // true = row in user_quotas, false = type default
}

// SetQuota upserts a per-user quota limit. limit 0 = unlimited.
func (s *Storage) SetQuota(ctx context.Context, actor, sig, pubkey, quotaTypeID string, limit int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := ensureUser(ctx, tx, pubkey); err != nil {
		return err
	}

	var prev sql.NullInt64
	_ = tx.QueryRowContext(ctx, `SELECT quota_limit FROM user_quotas WHERE pubkey = $1 AND quota_type_id = $2`, pubkey, quotaTypeID).Scan(&prev)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO user_quotas (pubkey, quota_type_id, quota_limit, last_updated)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (pubkey, quota_type_id) DO UPDATE SET quota_limit = $3, last_updated = NOW()
	`, pubkey, quotaTypeID, limit)
	if err != nil {
		return fmt.Errorf("set quota: %w", err)
	}

	var oldVal any
	if prev.Valid {
		oldVal = map[string]any{"quota_limit": prev.Int64}
	}
	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:   "user_quotas",
		RecordID:    pubkey + ":" + quotaTypeID,
		Action:      "quota.set",
		ActorPubkey: actor,
		OldValues:   oldVal,
		NewValues:   map[string]any{"quota_limit": limit},
		Metadata:    map[string]any{"source": "admin"},
		Signature:   sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetQuota removes a per-user override so the type default applies.
func (s *Storage) ResetQuota(ctx context.Context, actor, sig, pubkey, quotaTypeID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `DELETE FROM user_quotas WHERE pubkey = $1 AND quota_type_id = $2`, pubkey, quotaTypeID)
	if err != nil {
		return fmt.Errorf("reset quota: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:   "user_quotas",
		RecordID:    pubkey + ":" + quotaTypeID,
		Action:      "quota.reset",
		ActorPubkey: actor,
		Metadata:    map[string]any{"source": "admin"},
		Signature:   sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// GetQuotas returns all quota types with the user's effective limits.
func (s *Storage) GetQuotas(ctx context.Context, pubkey string) ([]QuotaView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT qt.id, qt.display_name, qt.unit,
		       COALESCE(uq.quota_limit, qt.default_limit) AS quota_limit,
		       COALESCE(uq.current_usage, 0) AS current_usage,
		       (uq.pubkey IS NOT NULL) AS is_override
		FROM quota_types qt
		LEFT JOIN user_quotas uq ON uq.quota_type_id = qt.id AND uq.pubkey = $1
		ORDER BY qt.id
	`, pubkey)
	if err != nil {
		return nil, fmt.Errorf("get quotas: %w", err)
	}
	defer rows.Close()

	var out []QuotaView
	for rows.Next() {
		var q QuotaView
		if err := rows.Scan(&q.QuotaTypeID, &q.DisplayName, &q.Unit, &q.QuotaLimit, &q.CurrentUsage, &q.IsOverride); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Username tiers
// ---------------------------------------------------------------------------

// ListTiers returns all pricing tiers.
func (s *Storage) ListTiers(ctx context.Context) ([]UsernameTier, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tier_name, min_length, max_length, price_sats, enabled
		FROM username_tiers ORDER BY min_length
	`)
	if err != nil {
		return nil, fmt.Errorf("list tiers: %w", err)
	}
	defer rows.Close()

	var out []UsernameTier
	for rows.Next() {
		var t UsernameTier
		if err := rows.Scan(&t.ID, &t.TierName, &t.MinLength, &t.MaxLength, &t.PriceSats, &t.Enabled); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTier changes a tier's price and enabled flag.
func (s *Storage) UpdateTier(ctx context.Context, actor, sig, tierName string, priceSats int64, enabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var prevPrice int64
	var prevEnabled bool
	err = tx.QueryRowContext(ctx, `SELECT price_sats, enabled FROM username_tiers WHERE tier_name = $1`, tierName).Scan(&prevPrice, &prevEnabled)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup tier: %w", err)
	}

	_, err = tx.ExecContext(ctx, `UPDATE username_tiers SET price_sats = $2, enabled = $3 WHERE tier_name = $1`, tierName, priceSats, enabled)
	if err != nil {
		return fmt.Errorf("update tier: %w", err)
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:   "username_tiers",
		RecordID:    tierName,
		Action:      "tier.update",
		ActorPubkey: actor,
		OldValues:   map[string]any{"price_sats": prevPrice, "enabled": prevEnabled},
		NewValues:   map[string]any{"price_sats": priceSats, "enabled": enabled},
		Metadata:    map[string]any{"source": "admin"},
		Signature:   sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

// AuditRow is a returned audit_log entry.
type AuditRow struct {
	ID          int64           `json:"id"`
	TableName   string          `json:"table_name"`
	RecordID    *string         `json:"record_id,omitempty"`
	Action      string          `json:"action"`
	ActorPubkey *string         `json:"actor_pubkey,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	OldValues   json.RawMessage `json:"old_values,omitempty"`
	NewValues   json.RawMessage `json:"new_values,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Signature   *string         `json:"signature,omitempty"`
	PrevHash    *string         `json:"prev_hash,omitempty"`
	EntryHash   *string         `json:"entry_hash,omitempty"`
}

// ListAudit returns recent audit entries, newest first, optionally filtered.
func (s *Storage) ListAudit(ctx context.Context, action, actor string, limit, offset int) ([]AuditRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, table_name, record_id, action, actor_pubkey, created_at,
		       old_values, new_values, metadata, signature, prev_hash, entry_hash
		FROM audit_log
		WHERE ($1 = '' OR action = $1)
		  AND ($2 = '' OR actor_pubkey = $2)
		ORDER BY id DESC
		LIMIT $3 OFFSET $4
	`, action, actor, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()

	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.TableName, &r.RecordID, &r.Action, &r.ActorPubkey, &r.CreatedAt,
			&r.OldValues, &r.NewValues, &r.Metadata, &r.Signature, &r.PrevHash, &r.EntryHash); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
