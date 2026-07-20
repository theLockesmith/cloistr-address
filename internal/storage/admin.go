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
	TableName     string
	RecordID      string
	Action        string
	ActorPubkey   string
	SubjectPubkey string // who the action was about (indexed; not in chain hash)
	OldValues     any    // marshaled to JSONB (nil = NULL)
	NewValues     any    // marshaled to JSONB (nil = NULL)
	Metadata      any    // {source, user_agent, request_id, ...}
	Signature     string // NIP-98 schnorr sig of the admin action (hex)
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

	// subject_pubkey is stored as NULL when empty so the index stays selective.
	var subjectPubkey interface{}
	if e.SubjectPubkey != "" {
		subjectPubkey = e.SubjectPubkey
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_log
			(table_name, record_id, action, actor_pubkey, actor_type,
			 old_values, new_values, metadata, signature, subject_pubkey)
		VALUES ($1, $2, $3, $4, 'user', $5, $6, $7, $8, $9)
	`, e.TableName, e.RecordID, e.Action, e.ActorPubkey, oldV, newV, meta, e.Signature, subjectPubkey)
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
// reserved-list. Supports N-addresses per pubkey:
//   - First address for the pubkey: is_primary=TRUE, nip05_active=TRUE.
//   - Subsequent addresses (aliases): is_primary=FALSE, nip05_active=FALSE;
//     LN config is copied from the pubkey's primary address if one exists.
//
// The audit entry SubjectPubkey is set to the grantee pubkey.
func (s *Storage) AdminGrantAddress(ctx context.Context, actor, sig, username, domain, pubkey string, displayName *string) (*Address, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := ensureUser(ctx, tx, pubkey); err != nil {
		return nil, err
	}

	// Is this the first active address for this pubkey?
	var existingCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE
	`, pubkey).Scan(&existingCount); err != nil {
		return nil, fmt.Errorf("count existing addresses: %w", err)
	}
	isFirst := existingCount == 0

	// For aliases, remember the primary address ID to inherit LN config.
	var primaryAddrID int64
	if !isFirst {
		_ = tx.QueryRowContext(ctx, `
			SELECT id FROM addresses WHERE pubkey = $1 AND is_primary = TRUE AND active = TRUE LIMIT 1
		`, pubkey).Scan(&primaryAddrID)
	}

	addr := &Address{}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO addresses
			(username, domain, pubkey, active, verified, display_name,
			 is_primary, nip05_active, created_at, updated_at)
		VALUES ($1, $2, $3, true, true, $4, $5, $5, NOW(), NOW())
		RETURNING id, username, domain, pubkey, active, verified,
		          is_primary, nip05_active, display_name, created_at, updated_at
	`, username, domain, pubkey, displayName, isFirst).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified,
		&addr.IsPrimary, &addr.NIP05Active,
		&addr.DisplayName, &addr.CreatedAt, &addr.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert address: %w", err)
	}

	// Open ownership interval.
	if err := openOwnership(ctx, tx, username, domain, pubkey); err != nil {
		return nil, err
	}

	// Inherit LN config for aliases.
	if !isFirst && primaryAddrID != 0 {
		if err := copyLightningConfig(ctx, tx, primaryAddrID, addr.ID); err != nil {
			// Non-fatal: log but don't abort the grant.
			_ = err // suppress unused error
		}
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:     "addresses",
		RecordID:      fmt.Sprintf("%d", addr.ID),
		Action:        "address.grant",
		ActorPubkey:   actor,
		SubjectPubkey: pubkey,
		NewValues: map[string]any{
			"username":     username,
			"domain":       domain,
			"pubkey":       pubkey,
			"display_name": displayName,
			"is_primary":   isFirst,
			"nip05_active": isFirst,
		},
		Metadata:  map[string]any{"source": "admin"},
		Signature: sig,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return addr, nil
}

// AdminRevokeAddress deactivates an address (soft-delete) and records a ban reason.
// If the address was primary or nip05_active for its pubkey, and the pubkey still
// has other active addresses, the oldest remaining address is promoted.
// Closes the ownership interval.
func (s *Storage) AdminRevokeAddress(ctx context.Context, actor, sig, username, domain, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	var prevPubkey string
	var wasPrimary, wasNIP05Active bool
	err = tx.QueryRowContext(ctx, `
		UPDATE addresses SET active = false, ban_reason = $3, updated_at = NOW()
		WHERE username = $1 AND domain = $2
		RETURNING id, pubkey, is_primary, nip05_active
	`, username, domain, reason).Scan(&id, &prevPubkey, &wasPrimary, &wasNIP05Active)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke address: %w", err)
	}

	// Close ownership interval.
	if err := closeOwnership(ctx, tx, username, domain, prevPubkey); err != nil {
		return err
	}

	// If this was the primary, promote another active address.
	if wasPrimary {
		var others int
		_ = tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE
		`, prevPubkey).Scan(&others)
		if others > 0 {
			if err := promotePrimaryAddress(ctx, tx, prevPubkey, id); err != nil {
				return fmt.Errorf("promote primary on revoke: %w", err)
			}
		}
	}
	// If this was nip05_active, promote another.
	if wasNIP05Active {
		var others int
		_ = tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE
		`, prevPubkey).Scan(&others)
		if others > 0 {
			if err := promoteNIP05Address(ctx, tx, prevPubkey, id); err != nil {
				return fmt.Errorf("promote nip05 on revoke: %w", err)
			}
		}
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:     "addresses",
		RecordID:      fmt.Sprintf("%d", id),
		Action:        "address.revoke",
		ActorPubkey:   actor,
		SubjectPubkey: prevPubkey,
		OldValues:     map[string]any{"active": true, "pubkey": prevPubkey, "is_primary": wasPrimary, "nip05_active": wasNIP05Active},
		NewValues:     map[string]any{"active": false, "ban_reason": reason},
		Metadata:      map[string]any{"source": "admin"},
		Signature:     sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// AdminTransferAddress moves an address to a new pubkey (admin override).
// Handles is_primary / nip05_active reassignment for both pubkeys and ownership intervals.
// SubjectPubkey in the audit row is set to toPubkey; the fromPubkey is captured in OldValues.
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
	var wasPrimary, wasNIP05Active bool
	err = tx.QueryRowContext(ctx, `
		SELECT id, pubkey, is_primary, nip05_active
		FROM addresses WHERE username = $1 AND domain = $2
	`, username, domain).Scan(&id, &fromPubkey, &wasPrimary, &wasNIP05Active)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup address: %w", err)
	}

	// Count OTHER active addresses of the source pubkey (excluding this one).
	var fromOthers int
	_ = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE AND id != $2
	`, fromPubkey, id).Scan(&fromOthers)

	// Is this the target's first active address?
	var toCount int
	_ = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE
	`, toPubkey).Scan(&toCount)
	isFirstForTarget := toCount == 0

	// Transfer and set flags.
	_, err = tx.ExecContext(ctx, `
		UPDATE addresses
		SET pubkey = $2, is_primary = $3, nip05_active = $3,
		    last_transfer_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, id, toPubkey, isFirstForTarget)
	if err != nil {
		return fmt.Errorf("transfer address: %w", err)
	}

	// Promote another address for the source pubkey if this was its primary.
	if wasPrimary && fromOthers > 0 {
		if err := promotePrimaryAddress(ctx, tx, fromPubkey, id); err != nil {
			return fmt.Errorf("promote primary after admin transfer: %w", err)
		}
	}
	if wasNIP05Active && fromOthers > 0 {
		if err := promoteNIP05Address(ctx, tx, fromPubkey, id); err != nil {
			return fmt.Errorf("promote nip05 after admin transfer: %w", err)
		}
	}

	// Transition ownership interval.
	if err := closeOwnership(ctx, tx, username, domain, fromPubkey); err != nil {
		return err
	}
	if err := openOwnership(ctx, tx, username, domain, toPubkey); err != nil {
		return err
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:     "addresses",
		RecordID:      fmt.Sprintf("%d", id),
		Action:        "address.transfer",
		ActorPubkey:   actor,
		SubjectPubkey: toPubkey,
		OldValues:     map[string]any{"pubkey": fromPubkey},
		NewValues:     map[string]any{"pubkey": toPubkey, "is_primary": isFirstForTarget},
		Metadata:      map[string]any{"source": "admin"},
		Signature:     sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// AdminListAddressesByPubkey returns all addresses (active or not) for a pubkey.
func (s *Storage) AdminListAddressesByPubkey(ctx context.Context, pubkey string) ([]Address, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified, is_primary, nip05_active,
		       display_name, created_at, updated_at, ban_reason
		FROM addresses WHERE pubkey = $1 ORDER BY created_at
	`, pubkey)
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	defer rows.Close()

	var out []Address
	for rows.Next() {
		var a Address
		if err := rows.Scan(
			&a.ID, &a.Username, &a.Domain, &a.Pubkey,
			&a.Active, &a.Verified, &a.IsPrimary, &a.NIP05Active,
			&a.DisplayName, &a.CreatedAt, &a.UpdatedAt, &a.BanReason,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAddressPrimary atomically sets the is_primary flag on the given address
// for pubkey, unsetting it on any current primary first. The address must be
// active and belong to pubkey.
func (s *Storage) SetAddressPrimary(ctx context.Context, actor, sig, username, domain, pubkey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var addrID int64
	var addrPubkey string
	err = tx.QueryRowContext(ctx, `
		SELECT id, pubkey FROM addresses WHERE username = $1 AND domain = $2 AND active = TRUE
	`, username, domain).Scan(&addrID, &addrPubkey)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup address: %w", err)
	}
	if addrPubkey != pubkey {
		return fmt.Errorf("address %s@%s does not belong to pubkey", username, domain)
	}

	// Unset current primary for this pubkey.
	if _, err := tx.ExecContext(ctx, `
		UPDATE addresses SET is_primary = FALSE, updated_at = NOW()
		WHERE pubkey = $1 AND is_primary = TRUE
	`, pubkey); err != nil {
		return fmt.Errorf("unset primary: %w", err)
	}

	// Set new primary.
	if _, err := tx.ExecContext(ctx, `
		UPDATE addresses SET is_primary = TRUE, updated_at = NOW() WHERE id = $1
	`, addrID); err != nil {
		return fmt.Errorf("set primary: %w", err)
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:     "addresses",
		RecordID:      fmt.Sprintf("%d", addrID),
		Action:        "address.set_primary",
		ActorPubkey:   actor,
		SubjectPubkey: pubkey,
		NewValues:     map[string]any{"username": username, "domain": domain, "is_primary": true},
		Metadata:      map[string]any{"source": "admin"},
		Signature:     sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAddressNIP05 atomically sets the nip05_active flag on the given address
// for pubkey, unsetting it on any current nip05_active address first. The
// address must be active and belong to pubkey.
func (s *Storage) SetAddressNIP05(ctx context.Context, actor, sig, username, domain, pubkey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var addrID int64
	var addrPubkey string
	err = tx.QueryRowContext(ctx, `
		SELECT id, pubkey FROM addresses WHERE username = $1 AND domain = $2 AND active = TRUE
	`, username, domain).Scan(&addrID, &addrPubkey)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup address: %w", err)
	}
	if addrPubkey != pubkey {
		return fmt.Errorf("address %s@%s does not belong to pubkey", username, domain)
	}

	// Unset current nip05_active for this pubkey.
	if _, err := tx.ExecContext(ctx, `
		UPDATE addresses SET nip05_active = FALSE, updated_at = NOW()
		WHERE pubkey = $1 AND nip05_active = TRUE
	`, pubkey); err != nil {
		return fmt.Errorf("unset nip05_active: %w", err)
	}

	// Set new nip05_active.
	if _, err := tx.ExecContext(ctx, `
		UPDATE addresses SET nip05_active = TRUE, updated_at = NOW() WHERE id = $1
	`, addrID); err != nil {
		return fmt.Errorf("set nip05_active: %w", err)
	}

	if err := writeAudit(ctx, tx, AuditEntry{
		TableName:     "addresses",
		RecordID:      fmt.Sprintf("%d", addrID),
		Action:        "address.set_nip05",
		ActorPubkey:   actor,
		SubjectPubkey: pubkey,
		NewValues:     map[string]any{"username": username, "domain": domain, "nip05_active": true},
		Metadata:      map[string]any{"source": "admin"},
		Signature:     sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// User lookup (name → pubkey + addresses)
// ---------------------------------------------------------------------------

// AddressSummary is a compact address entry used in admin lookup responses.
type AddressSummary struct {
	Username    string    `json:"username"`
	Domain      string    `json:"domain"`
	Active      bool      `json:"active"`
	IsPrimary   bool      `json:"is_primary"`
	NIP05Active bool      `json:"nip05_active"`
	DisplayName *string   `json:"display_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// UserLookup is the response for the admin name-to-pubkey resolver.
type UserLookup struct {
	Pubkey    string           `json:"pubkey"`
	Addresses []AddressSummary `json:"addresses"`
}

// AdminLookupUser resolves a canonical username+domain to a pubkey and returns
// all active addresses for that pubkey. Returns nil (not an error) when unknown.
func (s *Storage) AdminLookupUser(ctx context.Context, name, domain string) (*UserLookup, error) {
	// Find the pubkey currently holding this name.
	var pubkey string
	q := `SELECT pubkey FROM addresses WHERE username = $1 AND active = TRUE`
	args := []any{name}
	if domain != "" {
		q += ` AND domain = $2`
		args = append(args, domain)
	}
	q += ` LIMIT 1`

	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&pubkey); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("lookup user: %w", err)
	}

	// Return all active addresses for that pubkey.
	rows, err := s.db.QueryContext(ctx, `
		SELECT username, domain, active, is_primary, nip05_active, display_name, created_at
		FROM addresses
		WHERE pubkey = $1 AND active = TRUE
		ORDER BY is_primary DESC, created_at ASC
	`, pubkey)
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	defer rows.Close()

	var addrs []AddressSummary
	for rows.Next() {
		var a AddressSummary
		if err := rows.Scan(&a.Username, &a.Domain, &a.Active, &a.IsPrimary, &a.NIP05Active, &a.DisplayName, &a.CreatedAt); err != nil {
			return nil, err
		}
		addrs = append(addrs, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &UserLookup{Pubkey: pubkey, Addresses: addrs}, nil
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
		TableName:     "user_quotas",
		RecordID:      pubkey + ":" + quotaTypeID,
		Action:        "quota.set",
		ActorPubkey:   actor,
		SubjectPubkey: pubkey,
		OldValues:     oldVal,
		NewValues:     map[string]any{"quota_limit": limit},
		Metadata:      map[string]any{"source": "admin"},
		Signature:     sig,
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
		TableName:     "user_quotas",
		RecordID:      pubkey + ":" + quotaTypeID,
		Action:        "quota.reset",
		ActorPubkey:   actor,
		SubjectPubkey: pubkey,
		Metadata:      map[string]any{"source": "admin"},
		Signature:     sig,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// GetQuotas returns all quota types with the user's effective limits.
func (s *Storage) GetQuotas(ctx context.Context, pubkey string) ([]QuotaView, error) {
	// effective_quota() resolves the identity-scaled limit + non-expired grants and
	// the SUM of per-service usage (user_quota_usage) — the same values enforcement
	// uses — so the admin view matches what services actually enforce. is_override
	// still reflects whether an explicit user_quotas row exists.
	rows, err := s.db.QueryContext(ctx, `
		SELECT qt.id, qt.display_name, qt.unit,
		       eq.quota_limit, eq.current_usage,
		       (uq.pubkey IS NOT NULL) AS is_override
		FROM quota_types qt
		CROSS JOIN LATERAL effective_quota($1, qt.id) eq
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
	ID            int64           `json:"id"`
	TableName     string          `json:"table_name"`
	RecordID      *string         `json:"record_id,omitempty"`
	Action        string          `json:"action"`
	ActorPubkey   *string         `json:"actor_pubkey,omitempty"`
	SubjectPubkey *string         `json:"subject_pubkey,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	OldValues     json.RawMessage `json:"old_values,omitempty"`
	NewValues     json.RawMessage `json:"new_values,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	Signature     *string         `json:"signature,omitempty"`
	PrevHash      *string         `json:"prev_hash,omitempty"`
	EntryHash     *string         `json:"entry_hash,omitempty"`
}

// AuditListFilter holds filter criteria for ListAudit.
type AuditListFilter struct {
	// Action and actor narrow by exact match ("" = no filter).
	Action string
	Actor  string

	// Subject matches audit_log.subject_pubkey directly.
	Subject string

	// Name + Domain resolve via address_ownership temporal join:
	// the audit row's created_at must fall within the ownership interval
	// for (Name, Domain) → subject_pubkey.
	Name   string
	Domain string

	// Start / End filter audit_log.created_at (nil = no bound).
	Start *time.Time
	End   *time.Time

	Limit  int
	Offset int
}

// ListAudit returns audit entries newest-first, applying all set filters.
// Subject and Name filters are unioned (either match qualifies).
func (s *Storage) ListAudit(ctx context.Context, f AuditListFilter) ([]AuditRow, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}

	// Build the subject/name predicate dynamically to avoid always-false branches.
	// $1...$4 are action, actor, start, end.
	// The subject/name block is $5, $6, $7.
	var subjectClause string
	switch {
	case f.Subject == "" && f.Name == "":
		subjectClause = "TRUE" // no filter
	case f.Subject != "" && f.Name == "":
		subjectClause = "al.subject_pubkey = $5"
	case f.Subject == "" && f.Name != "":
		subjectClause = `EXISTS (
			SELECT 1 FROM address_ownership ao
			WHERE ao.username = $6
			  AND ($7 = '' OR ao.domain = $7)
			  AND ao.pubkey = al.subject_pubkey
			  AND ao.valid_from <= al.created_at
			  AND (ao.valid_to IS NULL OR ao.valid_to > al.created_at)
		)`
	default: // both set — union
		subjectClause = `(al.subject_pubkey = $5 OR EXISTS (
			SELECT 1 FROM address_ownership ao
			WHERE ao.username = $6
			  AND ($7 = '' OR ao.domain = $7)
			  AND ao.pubkey = al.subject_pubkey
			  AND ao.valid_from <= al.created_at
			  AND (ao.valid_to IS NULL OR ao.valid_to > al.created_at)
		))`
	}

	q := fmt.Sprintf(`
		SELECT al.id, al.table_name, al.record_id, al.action, al.actor_pubkey,
		       al.subject_pubkey, al.created_at,
		       al.old_values, al.new_values, al.metadata,
		       al.signature, al.prev_hash, al.entry_hash
		FROM audit_log al
		WHERE ($1 = '' OR al.action = $1)
		  AND ($2 = '' OR al.actor_pubkey = $2)
		  AND ($3::TIMESTAMPTZ IS NULL OR al.created_at >= $3)
		  AND ($4::TIMESTAMPTZ IS NULL OR al.created_at <= $4)
		  AND (%s)
		ORDER BY al.id DESC
		LIMIT $8 OFFSET $9
	`, subjectClause)

	// Always pass all 9 positional params; unused ones are ignored by the DB.
	var startVal, endVal interface{}
	if f.Start != nil {
		startVal = *f.Start
	}
	if f.End != nil {
		endVal = *f.End
	}

	rows, err := s.db.QueryContext(ctx, q,
		f.Action, f.Actor, startVal, endVal,
		f.Subject, f.Name, f.Domain,
		f.Limit, f.Offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()

	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(
			&r.ID, &r.TableName, &r.RecordID, &r.Action, &r.ActorPubkey,
			&r.SubjectPubkey, &r.CreatedAt,
			&r.OldValues, &r.NewValues, &r.Metadata,
			&r.Signature, &r.PrevHash, &r.EntryHash,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

