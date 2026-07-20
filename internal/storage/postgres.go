package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/lib/pq"

	"git.aegis-hq.xyz/coldforge/cloistr-common/username"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
)

// Storage handles database operations
type Storage struct {
	db *sql.DB
}

// Address represents a user's address record.
// JSON tags use snake_case so the admin UI gets predictable field names.
type Address struct {
	ID              int64      `json:"id"`
	Username        string     `json:"username"`
	Domain          string     `json:"domain"`
	Pubkey          string     `json:"pubkey"`
	Active          bool       `json:"active"`
	Verified        bool       `json:"verified"`
	IsPrimary       bool       `json:"is_primary"`
	NIP05Active     bool       `json:"nip05_active"`
	DisplayName     *string    `json:"display_name,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	GracePeriodEnds *time.Time `json:"grace_period_ends,omitempty"`
	BanReason       *string    `json:"ban_reason,omitempty"`
	LastTransferAt  *time.Time `json:"last_transfer_at,omitempty"`
}

// AddressRelay represents a relay URL associated with an address
type AddressRelay struct {
	ID        int64
	AddressID int64
	RelayURL  string
	Priority  int
}

// AddressLightning represents Lightning Address configuration
type AddressLightning struct {
	AddressID          int64
	Mode               string // "proxy", "nwc", "hosted", "disabled"
	ProxyAddress       string
	NWCConnection      string // Deprecated: use NWCSecretEncrypted
	NWCRelayURL        string
	NWCWalletPubkey    string
	NWCSecretEncrypted string
	NWCLastSuccessAt   *time.Time
	NWCLastError       *string
	NWCErrorCount      int
	MinSendableMsats   int64
	MaxSendableMsats   int64
	CommentAllowed     int
	AllowsNostr        bool
	Enabled            bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// UsernameTier represents pricing tier for usernames
type UsernameTier struct {
	ID        int64
	TierName  string
	MinLength int
	MaxLength *int
	PriceSats int64
	Enabled   bool
}

// CreditWithdrawal represents a withdrawal request
type CreditWithdrawal struct {
	ID               int64
	Pubkey           string
	AmountSats       int64
	LightningAddress string
	Status           string // pending, processing, completed, failed
	PaymentHash      *string
	ErrorMessage     *string
	CreatedAt        time.Time
	CompletedAt      *time.Time
}

// New creates a new storage instance
func New(cfg config.DatabaseConfig) (*Storage, error) {
	db, err := sql.Open("postgres", cfg.ConnectionString())
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	slog.Info("connected to database")
	return &Storage{db: db}, nil
}

// Close closes the database connection
func (s *Storage) Close() error {
	return s.db.Close()
}

// GetAddressByUsername retrieves an active address by username and domain.
// NIP-05 and LNURLP use this; each (username, domain) is unique among active rows.
func (s *Storage) GetAddressByUsername(ctx context.Context, username, domain string) (*Address, error) {
	addr := &Address{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified, is_primary, nip05_active,
		       display_name, created_at, updated_at, expires_at, grace_period_ends, ban_reason
		FROM addresses
		WHERE username = $1 AND domain = $2 AND active = true
	`, username, domain).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.IsPrimary, &addr.NIP05Active,
		&addr.DisplayName, &addr.CreatedAt, &addr.UpdatedAt,
		&addr.ExpiresAt, &addr.GracePeriodEnds, &addr.BanReason,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}
	return addr, nil
}

// GetAddressByPubkey returns one active address for pubkey, preferring the
// primary address. Kept for backwards compatibility; prefer GetPrimaryAddressByPubkey
// where exact primary semantics matter.
func (s *Storage) GetAddressByPubkey(ctx context.Context, pubkey string) (*Address, error) {
	addr := &Address{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified, is_primary, nip05_active,
		       display_name, created_at, updated_at, expires_at, grace_period_ends, ban_reason
		FROM addresses
		WHERE pubkey = $1 AND active = true
		ORDER BY is_primary DESC, id ASC
		LIMIT 1
	`, pubkey).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.IsPrimary, &addr.NIP05Active,
		&addr.DisplayName, &addr.CreatedAt, &addr.UpdatedAt,
		&addr.ExpiresAt, &addr.GracePeriodEnds, &addr.BanReason,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}
	return addr, nil
}

// GetPrimaryAddressByPubkey retrieves the address that has is_primary=TRUE for pubkey.
// Returns nil (not an error) when the pubkey has no active primary address.
func (s *Storage) GetPrimaryAddressByPubkey(ctx context.Context, pubkey string) (*Address, error) {
	addr := &Address{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified, is_primary, nip05_active,
		       display_name, created_at, updated_at, expires_at, grace_period_ends, ban_reason
		FROM addresses
		WHERE pubkey = $1 AND active = true AND is_primary = true
	`, pubkey).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.IsPrimary, &addr.NIP05Active,
		&addr.DisplayName, &addr.CreatedAt, &addr.UpdatedAt,
		&addr.ExpiresAt, &addr.GracePeriodEnds, &addr.BanReason,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get primary address: %w", err)
	}
	return addr, nil
}

// GetAddressesByPubkey returns all active addresses for a pubkey, primary first.
func (s *Storage) GetAddressesByPubkey(ctx context.Context, pubkey string) ([]Address, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified, is_primary, nip05_active,
		       display_name, created_at, updated_at, expires_at, grace_period_ends, ban_reason
		FROM addresses
		WHERE pubkey = $1 AND active = true
		ORDER BY is_primary DESC, created_at ASC
	`, pubkey)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses: %w", err)
	}
	defer rows.Close()

	var out []Address
	for rows.Next() {
		var a Address
		if err := rows.Scan(
			&a.ID, &a.Username, &a.Domain, &a.Pubkey,
			&a.Active, &a.Verified, &a.IsPrimary, &a.NIP05Active,
			&a.DisplayName, &a.CreatedAt, &a.UpdatedAt,
			&a.ExpiresAt, &a.GracePeriodEnds, &a.BanReason,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetRelaysForAddress retrieves relay URLs for an address
func (s *Storage) GetRelaysForAddress(ctx context.Context, addressID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT relay_url FROM address_relays
		WHERE address_id = $1
		ORDER BY priority ASC
	`, addressID)
	if err != nil {
		return nil, fmt.Errorf("failed to get relays: %w", err)
	}
	defer rows.Close()

	var relays []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, fmt.Errorf("failed to scan relay: %w", err)
		}
		relays = append(relays, url)
	}
	return relays, nil
}

// GetLightningConfig retrieves Lightning Address configuration for an address
func (s *Storage) GetLightningConfig(ctx context.Context, addressID int64) (*AddressLightning, error) {
	ln := &AddressLightning{}
	var proxyAddr, nwcConn, nwcRelayURL, nwcWalletPubkey, nwcSecretEnc, nwcLastError sql.NullString
	var nwcLastSuccess sql.NullTime
	var nwcErrorCount sql.NullInt32
	err := s.db.QueryRowContext(ctx, `
		SELECT address_id, mode, proxy_address, nwc_connection,
		       COALESCE(nwc_relay_url, ''), COALESCE(nwc_wallet_pubkey, ''), COALESCE(nwc_secret_encrypted, ''),
		       nwc_last_success_at, nwc_last_error, COALESCE(nwc_error_count, 0),
		       min_sendable_msats, max_sendable_msats, comment_allowed,
		       allows_nostr, enabled, created_at, updated_at
		FROM address_lightning
		WHERE address_id = $1
	`, addressID).Scan(
		&ln.AddressID, &ln.Mode, &proxyAddr, &nwcConn,
		&nwcRelayURL, &nwcWalletPubkey, &nwcSecretEnc,
		&nwcLastSuccess, &nwcLastError, &nwcErrorCount,
		&ln.MinSendableMsats, &ln.MaxSendableMsats, &ln.CommentAllowed,
		&ln.AllowsNostr, &ln.Enabled, &ln.CreatedAt, &ln.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get lightning config: %w", err)
	}
	ln.ProxyAddress = proxyAddr.String
	ln.NWCConnection = nwcConn.String
	ln.NWCRelayURL = nwcRelayURL.String
	ln.NWCWalletPubkey = nwcWalletPubkey.String
	ln.NWCSecretEncrypted = nwcSecretEnc.String
	if nwcLastSuccess.Valid {
		ln.NWCLastSuccessAt = &nwcLastSuccess.Time
	}
	if nwcLastError.Valid {
		ln.NWCLastError = &nwcLastError.String
	}
	ln.NWCErrorCount = int(nwcErrorCount.Int32)
	return ln, nil
}

// UpdateNWCSuccess updates the NWC success timestamp and resets error count
func (s *Storage) UpdateNWCSuccess(ctx context.Context, addressID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE address_lightning
		SET nwc_last_success_at = NOW(),
		    nwc_error_count = 0,
		    nwc_last_error = NULL,
		    updated_at = NOW()
		WHERE address_id = $1
	`, addressID)
	if err != nil {
		slog.Warn("failed to update NWC success", "address_id", addressID, "error", err)
	}
	return err
}

// UpdateNWCError updates the NWC error tracking
func (s *Storage) UpdateNWCError(ctx context.Context, addressID int64, errorMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE address_lightning
		SET nwc_last_error = $2,
		    nwc_error_count = COALESCE(nwc_error_count, 0) + 1,
		    updated_at = NOW()
		WHERE address_id = $1
	`, addressID, errorMsg)
	if err != nil {
		slog.Warn("failed to update NWC error", "address_id", addressID, "error", err)
	}
	return err
}

// IsUsernameAvailable checks if a username is available for registration
func (s *Storage) IsUsernameAvailable(ctx context.Context, username string) (bool, error) {
	var available bool
	err := s.db.QueryRowContext(ctx, `SELECT is_username_available($1)`, username).Scan(&available)
	if err != nil {
		return false, fmt.Errorf("failed to check username availability: %w", err)
	}
	return available, nil
}

// CanRegisterUsername checks if a specific pubkey can register a username
func (s *Storage) CanRegisterUsername(ctx context.Context, username, pubkey string) (bool, error) {
	var canRegister bool
	err := s.db.QueryRowContext(ctx, `SELECT can_register_username($1, $2)`, username, pubkey).Scan(&canRegister)
	if err != nil {
		return false, fmt.Errorf("failed to check username registration: %w", err)
	}
	return canRegister, nil
}

// GetUsernamePrice returns the price in sats for a username based on length
func (s *Storage) GetUsernamePrice(ctx context.Context, usernameLength int) (int64, error) {
	var price int64
	err := s.db.QueryRowContext(ctx, `SELECT get_username_price($1)`, usernameLength).Scan(&price)
	if err != nil {
		return 0, fmt.Errorf("failed to get username price: %w", err)
	}
	return price, nil
}

// GetUsernameTier returns the tier name for a given username length
func (s *Storage) GetUsernameTier(ctx context.Context, usernameLength int) (string, error) {
	var tierName string
	err := s.db.QueryRowContext(ctx, `
		SELECT tier_name FROM username_tiers
		WHERE enabled = true
		  AND $1 >= min_length
		  AND (max_length IS NULL OR $1 <= max_length)
		ORDER BY price_sats DESC
		LIMIT 1
	`, usernameLength).Scan(&tierName)
	if err == sql.ErrNoRows {
		return "standard", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get username tier: %w", err)
	}
	return tierName, nil
}

// GetAllActiveAddresses retrieves all active addresses for NIP-05 bulk response.
// Note: with N-addresses per pubkey this may return multiple rows with the same
// pubkey (one per name). is_primary and nip05_active are included for completeness.
func (s *Storage) GetAllActiveAddresses(ctx context.Context, domain string) ([]*Address, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified, is_primary, nip05_active,
		       created_at, updated_at, expires_at, grace_period_ends, ban_reason
		FROM addresses
		WHERE domain = $1 AND active = true
		ORDER BY username
	`, domain)
	if err != nil {
		return nil, fmt.Errorf("failed to get addresses: %w", err)
	}
	defer rows.Close()

	var addresses []*Address
	for rows.Next() {
		addr := &Address{}
		if err := rows.Scan(
			&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
			&addr.Active, &addr.Verified, &addr.IsPrimary, &addr.NIP05Active,
			&addr.CreatedAt, &addr.UpdatedAt,
			&addr.ExpiresAt, &addr.GracePeriodEnds, &addr.BanReason,
		); err != nil {
			return nil, fmt.Errorf("failed to scan address: %w", err)
		}
		addresses = append(addresses, addr)
	}
	return addresses, nil
}

// GetAddressRelays retrieves relay URLs for an address (alias for GetRelaysForAddress)
func (s *Storage) GetAddressRelays(ctx context.Context, addressID int64) ([]string, error) {
	return s.GetRelaysForAddress(ctx, addressID)
}

// UpsertLightningConfig creates or updates Lightning Address configuration
// LightningConfigUpdate holds parameters for updating lightning config
type LightningConfigUpdate struct {
	Mode               string
	ProxyAddress       string
	NWCRelayURL        string
	NWCWalletPubkey    string
	NWCSecretEncrypted string
}

func (s *Storage) UpsertLightningConfig(ctx context.Context, addressID int64, mode, proxyAddress string) error {
	return s.UpsertLightningConfigFull(ctx, addressID, LightningConfigUpdate{
		Mode:         mode,
		ProxyAddress: proxyAddress,
	})
}

// UpsertLightningConfigFull upserts lightning config with all fields including NWC
func (s *Storage) UpsertLightningConfigFull(ctx context.Context, addressID int64, update LightningConfigUpdate) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO address_lightning (
			address_id, mode, proxy_address,
			nwc_relay_url, nwc_wallet_pubkey, nwc_secret_encrypted,
			nwc_error_count, enabled, updated_at
		)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), 0, true, NOW())
		ON CONFLICT (address_id) DO UPDATE SET
			mode = EXCLUDED.mode,
			proxy_address = CASE WHEN EXCLUDED.mode = 'proxy' THEN EXCLUDED.proxy_address ELSE NULL END,
			nwc_relay_url = CASE WHEN EXCLUDED.mode = 'nwc' THEN EXCLUDED.nwc_relay_url ELSE address_lightning.nwc_relay_url END,
			nwc_wallet_pubkey = CASE WHEN EXCLUDED.mode = 'nwc' THEN EXCLUDED.nwc_wallet_pubkey ELSE address_lightning.nwc_wallet_pubkey END,
			nwc_secret_encrypted = CASE WHEN EXCLUDED.mode = 'nwc' THEN EXCLUDED.nwc_secret_encrypted ELSE address_lightning.nwc_secret_encrypted END,
			nwc_error_count = CASE WHEN EXCLUDED.mode = 'nwc' THEN 0 ELSE address_lightning.nwc_error_count END,
			nwc_last_error = CASE WHEN EXCLUDED.mode = 'nwc' THEN NULL ELSE address_lightning.nwc_last_error END,
			enabled = CASE WHEN EXCLUDED.mode = 'disabled' THEN false ELSE true END,
			updated_at = NOW()
	`, addressID, update.Mode, update.ProxyAddress, update.NWCRelayURL, update.NWCWalletPubkey, update.NWCSecretEncrypted)
	if err != nil {
		return fmt.Errorf("failed to upsert lightning config: %w", err)
	}
	return nil
}

// TransferAddress transfers ownership of an address to a new pubkey.
// It handles is_primary / nip05_active reassignment for both the source and target
// pubkeys, and writes the ownership interval transition, all within a single tx.
func (s *Storage) TransferAddress(ctx context.Context, addressID int64, newPubkey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Ensure the target user row exists (FK guard).
	if err := ensureUser(ctx, tx, newPubkey); err != nil {
		return err
	}

	// Fetch current state of the address being transferred.
	var fromPubkey, username, domain string
	var wasPrimary, wasNIP05Active bool
	err = tx.QueryRowContext(ctx, `
		SELECT pubkey, username, domain, is_primary, nip05_active
		FROM addresses WHERE id = $1
	`, addressID).Scan(&fromPubkey, &username, &domain, &wasPrimary, &wasNIP05Active)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup address: %w", err)
	}

	// How many OTHER active addresses does the source pubkey still have after this transfer?
	var fromOthers int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE AND id != $2
	`, fromPubkey, addressID).Scan(&fromOthers); err != nil {
		return fmt.Errorf("count from-pubkey addresses: %w", err)
	}

	// Is this the target pubkey's first active address?
	var toCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE
	`, newPubkey).Scan(&toCount); err != nil {
		return fmt.Errorf("count to-pubkey addresses: %w", err)
	}
	isFirstForTarget := toCount == 0

	// Transfer the address row.
	_, err = tx.ExecContext(ctx, `
		UPDATE addresses
		SET pubkey = $2,
		    is_primary   = $3,
		    nip05_active = $3,
		    last_transfer_at = NOW(),
		    updated_at   = NOW()
		WHERE id = $1
	`, addressID, newPubkey, isFirstForTarget)
	if err != nil {
		return fmt.Errorf("transfer address: %w", err)
	}

	// Promote another address for the source pubkey if needed.
	if wasPrimary && fromOthers > 0 {
		if err := promotePrimaryAddress(ctx, tx, fromPubkey, addressID); err != nil {
			return fmt.Errorf("promote primary after transfer: %w", err)
		}
	}
	if wasNIP05Active && fromOthers > 0 {
		if err := promoteNIP05Address(ctx, tx, fromPubkey, addressID); err != nil {
			return fmt.Errorf("promote nip05 after transfer: %w", err)
		}
	}

	// Transition ownership interval.
	if err := closeOwnership(ctx, tx, username, domain, fromPubkey); err != nil {
		return fmt.Errorf("close ownership: %w", err)
	}
	if err := openOwnership(ctx, tx, username, domain, newPubkey); err != nil {
		return fmt.Errorf("open ownership: %w", err)
	}

	return tx.Commit()
}

// RegisterAddress registers a new address for a pubkey (simple, no uniqueness checks).
// Prefer AtomicRegisterAddress for production registration paths.
func (s *Storage) RegisterAddress(ctx context.Context, username, domain, pubkey string) (*Address, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var existingCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE
	`, pubkey).Scan(&existingCount); err != nil {
		return nil, fmt.Errorf("count existing: %w", err)
	}
	isFirst := existingCount == 0

	addr := &Address{}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO addresses (username, domain, pubkey, active, verified, is_primary, nip05_active, created_at, updated_at)
		VALUES ($1, $2, $3, true, false, $4, $4, NOW(), NOW())
		RETURNING id, username, domain, pubkey, active, verified, is_primary, nip05_active, created_at, updated_at
	`, username, domain, pubkey, isFirst).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.IsPrimary, &addr.NIP05Active,
		&addr.CreatedAt, &addr.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to register address: %w", err)
	}

	if err := openOwnership(ctx, tx, username, domain, pubkey); err != nil {
		return nil, fmt.Errorf("open ownership: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return addr, nil
}

// AtomicRegisterAddress attempts to register a username atomically.
// Returns the address if successful, nil if username was taken or reserved.
//
// Alias pricing: pricing is determined by username length via username_tiers
// (GetUsernamePrice / GetUsernameTier by the caller), so alias registrations
// are priced identically to first-address registrations for the same name length.
// No special-casing needed here.
func (s *Storage) AtomicRegisterAddress(ctx context.Context, username, domain, pubkey string, autoAssigned bool) (*Address, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Check availability within transaction
	var exists bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM addresses
			WHERE username = $1 AND domain = $2 AND active = true
		)
	`, username, domain).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("failed to check availability: %w", err)
	}
	if exists {
		return nil, nil // Username taken
	}

	// Check reserved
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM reserved_usernames WHERE username = $1)
	`, username).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("failed to check reserved: %w", err)
	}
	if exists {
		return nil, nil // Reserved
	}

	// Count the pubkey's existing active addresses, and how many are "real" (not
	// auto-assigned). This drives primary promotion: an anonymous user starts with
	// only an auto-assigned address; when they claim their first real name it must
	// become primary and the auto address is demoted to a plain alias (kept, so
	// anything already sent to it still delivers — pricing-model.md).
	var existingActive, existingRealActive int
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE COALESCE(auto_assigned, FALSE) = FALSE)
		FROM addresses WHERE pubkey = $1 AND active = TRUE
	`, pubkey).Scan(&existingActive, &existingRealActive); err != nil {
		return nil, fmt.Errorf("count existing addresses: %w", err)
	}
	isFirst := existingActive == 0
	// Promote when this is a real name and every existing active address is auto-assigned.
	promote := !isFirst && !autoAssigned && existingRealActive == 0
	makePrimary := isFirst || promote

	// Capture the current primary (for LN inheritance) before any demotion.
	var primaryAddrID int64
	if !isFirst {
		_ = tx.QueryRowContext(ctx, `
			SELECT id FROM addresses WHERE pubkey = $1 AND is_primary = TRUE AND active = TRUE LIMIT 1
		`, pubkey).Scan(&primaryAddrID)
	}

	// On promotion, demote the incumbent auto-assigned primary/NIP-05 first so the
	// one-primary / one-nip05 partial-unique indexes aren't violated by the insert.
	if promote {
		if _, err := tx.ExecContext(ctx, `
			UPDATE addresses SET is_primary = FALSE, nip05_active = FALSE, updated_at = NOW()
			WHERE pubkey = $1 AND active = TRUE AND (is_primary = TRUE OR nip05_active = TRUE)
		`, pubkey); err != nil {
			return nil, fmt.Errorf("demote auto-assigned primary: %w", err)
		}
	}

	// Register the address.
	addr := &Address{}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO addresses (username, domain, pubkey, active, verified, is_primary, nip05_active, auto_assigned, created_at, updated_at)
		VALUES ($1, $2, $3, true, false, $4, $4, $5, NOW(), NOW())
		RETURNING id, username, domain, pubkey, active, verified, is_primary, nip05_active, created_at, updated_at
	`, username, domain, pubkey, makePrimary, autoAssigned).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.IsPrimary, &addr.NIP05Active,
		&addr.CreatedAt, &addr.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to insert address: %w", err)
	}

	// Open ownership interval.
	if err := openOwnership(ctx, tx, username, domain, pubkey); err != nil {
		return nil, fmt.Errorf("open ownership: %w", err)
	}

	// Inherit LN config from the (former) primary when this address is NOT itself the
	// first primary — i.e. a plain alias, or a promoted name taking over from the auto
	// address. Keeps the wallet continuous across the anonymous→named transition.
	if !makePrimary && primaryAddrID != 0 || promote && primaryAddrID != 0 {
		if err := copyLightningConfig(ctx, tx, primaryAddrID, addr.ID); err != nil {
			// Non-fatal: log but don't fail registration.
			slog.Warn("failed to inherit lightning config",
				"source_id", primaryAddrID, "dest_id", addr.ID, "error", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit: %w", err)
	}

	return addr, nil
}

// EnsureUser idempotently creates the platform users row for pubkey so foreign keys
// (addresses.pubkey, user_quota_usage.pubkey, user_quotas.pubkey) hold and
// has_service_access() — which now checks users.enabled before the free-tier
// shortcut — doesn't lock out extension/NIP-07 users who never went through a
// registration flow. Safe to call on every authenticated request.
func (s *Storage) EnsureUser(ctx context.Context, pubkey string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (pubkey) VALUES ($1)
		ON CONFLICT (pubkey) DO NOTHING
	`, pubkey)
	if err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}
	return nil
}

// EnsureAutoAddress gives a nameless identity (one that has no active address) a
// readable auto-assigned adjective-noun-NNNN address so email delivery and Lightning
// zaps work at all. It is a no-op if the pubkey already has any active address. The
// generated name uses the reserved auto-assign shape (auto_assigned=TRUE), which does
// NOT confer the named storage tier. Best-effort: callers should treat errors as
// non-fatal. Returns (nil, nil) when nothing was created.
func (s *Storage) EnsureAutoAddress(ctx context.Context, pubkey, domain string) (*Address, error) {
	var active int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM addresses WHERE pubkey = $1 AND active = TRUE
	`, pubkey).Scan(&active); err != nil {
		return nil, fmt.Errorf("count addresses: %w", err)
	}
	if active > 0 {
		return nil, nil
	}

	name, err := username.Generate(func(candidate string) (bool, error) {
		return s.IsUsernameAvailable(ctx, candidate)
	})
	if err != nil {
		return nil, fmt.Errorf("generate auto address: %w", err)
	}
	return s.AtomicRegisterAddress(ctx, name, domain, pubkey, true)
}

// Credit operations

// GetCredits returns the credit balance for a pubkey
func (s *Storage) GetCredits(ctx context.Context, pubkey string) (int64, error) {
	var balance int64
	err := s.db.QueryRowContext(ctx, `
		SELECT balance_sats FROM pubkey_credits WHERE pubkey = $1
	`, pubkey).Scan(&balance)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get credits: %w", err)
	}
	return balance, nil
}

// AddCredits adds credits to a pubkey and records the transaction
func (s *Storage) AddCredits(ctx context.Context, pubkey string, amountSats int64, reason, referenceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Upsert credit balance
	_, err = tx.ExecContext(ctx, `
		INSERT INTO pubkey_credits (pubkey, balance_sats)
		VALUES ($1, $2)
		ON CONFLICT (pubkey) DO UPDATE SET
			balance_sats = pubkey_credits.balance_sats + $2,
			updated_at = NOW()
	`, pubkey, amountSats)
	if err != nil {
		return fmt.Errorf("failed to update credits: %w", err)
	}

	// Record history
	_, err = tx.ExecContext(ctx, `
		INSERT INTO credit_history (pubkey, amount_sats, reason, reference_id)
		VALUES ($1, $2, $3, $4)
	`, pubkey, amountSats, reason, referenceID)
	if err != nil {
		return fmt.Errorf("failed to record credit history: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	return nil
}

// DeductCredits deducts credits from a pubkey if sufficient balance
// Returns error if insufficient balance
func (s *Storage) DeductCredits(ctx context.Context, pubkey string, amountSats int64, reason, referenceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Check and deduct
	var newBalance int64
	err = tx.QueryRowContext(ctx, `
		UPDATE pubkey_credits
		SET balance_sats = balance_sats - $2, updated_at = NOW()
		WHERE pubkey = $1 AND balance_sats >= $2
		RETURNING balance_sats
	`, pubkey, amountSats).Scan(&newBalance)
	if err == sql.ErrNoRows {
		return ErrInsufficientCredits
	}
	if err != nil {
		return fmt.Errorf("failed to deduct credits: %w", err)
	}

	// Record history (negative amount)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO credit_history (pubkey, amount_sats, reason, reference_id)
		VALUES ($1, $2, $3, $4)
	`, pubkey, -amountSats, reason, referenceID)
	if err != nil {
		return fmt.Errorf("failed to record credit history: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	return nil
}

// CreateWithdrawalRequest creates a new withdrawal request
// Deducts credits atomically when creating the request
func (s *Storage) CreateWithdrawalRequest(ctx context.Context, pubkey string, amountSats int64, lightningAddress string) (*CreditWithdrawal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Check and deduct credits
	var newBalance int64
	err = tx.QueryRowContext(ctx, `
		UPDATE pubkey_credits
		SET balance_sats = balance_sats - $2, updated_at = NOW()
		WHERE pubkey = $1 AND balance_sats >= $2
		RETURNING balance_sats
	`, pubkey, amountSats).Scan(&newBalance)
	if err == sql.ErrNoRows {
		return nil, ErrInsufficientCredits
	}
	if err != nil {
		return nil, fmt.Errorf("failed to deduct credits: %w", err)
	}

	// Create withdrawal request
	withdrawal := &CreditWithdrawal{}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO credit_withdrawals (pubkey, amount_sats, lightning_address, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING id, pubkey, amount_sats, lightning_address, status, created_at
	`, pubkey, amountSats, lightningAddress).Scan(
		&withdrawal.ID, &withdrawal.Pubkey, &withdrawal.AmountSats,
		&withdrawal.LightningAddress, &withdrawal.Status, &withdrawal.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create withdrawal: %w", err)
	}

	// Record in credit history
	_, err = tx.ExecContext(ctx, `
		INSERT INTO credit_history (pubkey, amount_sats, reason, reference_id)
		VALUES ($1, $2, 'withdrawal_request', $3)
	`, pubkey, -amountSats, fmt.Sprintf("withdrawal_%d", withdrawal.ID))
	if err != nil {
		return nil, fmt.Errorf("failed to record credit history: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit: %w", err)
	}

	return withdrawal, nil
}

// GetPendingWithdrawals returns all pending withdrawal requests
func (s *Storage) GetPendingWithdrawals(ctx context.Context) ([]*CreditWithdrawal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, pubkey, amount_sats, lightning_address, status, payment_hash, error_message, created_at, completed_at
		FROM credit_withdrawals
		WHERE status = 'pending'
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending withdrawals: %w", err)
	}
	defer rows.Close()

	var withdrawals []*CreditWithdrawal
	for rows.Next() {
		w := &CreditWithdrawal{}
		var paymentHash, errorMsg sql.NullString
		var completedAt sql.NullTime
		if err := rows.Scan(
			&w.ID, &w.Pubkey, &w.AmountSats, &w.LightningAddress, &w.Status,
			&paymentHash, &errorMsg, &w.CreatedAt, &completedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan withdrawal: %w", err)
		}
		if paymentHash.Valid {
			w.PaymentHash = &paymentHash.String
		}
		if errorMsg.Valid {
			w.ErrorMessage = &errorMsg.String
		}
		if completedAt.Valid {
			w.CompletedAt = &completedAt.Time
		}
		withdrawals = append(withdrawals, w)
	}
	return withdrawals, nil
}

// UpdateWithdrawalStatus updates a withdrawal request status
func (s *Storage) UpdateWithdrawalStatus(ctx context.Context, id int64, status string, paymentHash, errorMessage *string) error {
	var completedAt interface{}
	if status == "completed" || status == "failed" {
		completedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE credit_withdrawals
		SET status = $2,
		    payment_hash = COALESCE($3, payment_hash),
		    error_message = COALESCE($4, error_message),
		    completed_at = COALESCE($5, completed_at)
		WHERE id = $1
	`, id, status, paymentHash, errorMessage, completedAt)
	if err != nil {
		return fmt.Errorf("failed to update withdrawal status: %w", err)
	}
	return nil
}

// RefundFailedWithdrawal returns credits for a failed withdrawal
func (s *Storage) RefundFailedWithdrawal(ctx context.Context, withdrawalID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get withdrawal details
	var pubkey string
	var amountSats int64
	err = tx.QueryRowContext(ctx, `
		SELECT pubkey, amount_sats FROM credit_withdrawals WHERE id = $1
	`, withdrawalID).Scan(&pubkey, &amountSats)
	if err != nil {
		return fmt.Errorf("failed to get withdrawal: %w", err)
	}

	// Add credits back
	_, err = tx.ExecContext(ctx, `
		UPDATE pubkey_credits
		SET balance_sats = balance_sats + $2, updated_at = NOW()
		WHERE pubkey = $1
	`, pubkey, amountSats)
	if err != nil {
		return fmt.Errorf("failed to refund credits: %w", err)
	}

	// Record in history
	_, err = tx.ExecContext(ctx, `
		INSERT INTO credit_history (pubkey, amount_sats, reason, reference_id)
		VALUES ($1, $2, 'withdrawal_refund', $3)
	`, pubkey, amountSats, fmt.Sprintf("withdrawal_%d", withdrawalID))
	if err != nil {
		return fmt.Errorf("failed to record refund: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	return nil
}

// GetWithdrawalsByPubkey returns withdrawal history for a pubkey
func (s *Storage) GetWithdrawalsByPubkey(ctx context.Context, pubkey string, limit int) ([]*CreditWithdrawal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, pubkey, amount_sats, lightning_address, status, payment_hash, error_message, created_at, completed_at
		FROM credit_withdrawals
		WHERE pubkey = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, pubkey, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get withdrawals: %w", err)
	}
	defer rows.Close()

	var withdrawals []*CreditWithdrawal
	for rows.Next() {
		w := &CreditWithdrawal{}
		var paymentHash, errorMsg sql.NullString
		var completedAt sql.NullTime
		if err := rows.Scan(
			&w.ID, &w.Pubkey, &w.AmountSats, &w.LightningAddress, &w.Status,
			&paymentHash, &errorMsg, &w.CreatedAt, &completedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan withdrawal: %w", err)
		}
		if paymentHash.Valid {
			w.PaymentHash = &paymentHash.String
		}
		if errorMsg.Valid {
			w.ErrorMessage = &errorMsg.String
		}
		if completedAt.Valid {
			w.CompletedAt = &completedAt.Time
		}
		withdrawals = append(withdrawals, w)
	}
	return withdrawals, nil
}

// promotePrimaryAddress sets is_primary=TRUE on the oldest active address for pubkey
// (excluding exceptID). Called when a primary address is revoked or transferred.
func promotePrimaryAddress(ctx context.Context, tx *sql.Tx, pubkey string, exceptID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE addresses SET is_primary = TRUE, updated_at = NOW()
		WHERE id = (
			SELECT id FROM addresses
			WHERE pubkey = $1 AND active = TRUE AND id != $2
			ORDER BY created_at ASC
			LIMIT 1
		)
	`, pubkey, exceptID)
	return err
}

// promoteNIP05Address sets nip05_active=TRUE on the oldest active address for pubkey
// (excluding exceptID). Called when the nip05_active address is revoked or transferred.
func promoteNIP05Address(ctx context.Context, tx *sql.Tx, pubkey string, exceptID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE addresses SET nip05_active = TRUE, updated_at = NOW()
		WHERE id = (
			SELECT id FROM addresses
			WHERE pubkey = $1 AND active = TRUE AND id != $2
			ORDER BY created_at ASC
			LIMIT 1
		)
	`, pubkey, exceptID)
	return err
}

// openOwnership inserts an open-ended ownership interval for (username, domain) → pubkey.
func openOwnership(ctx context.Context, tx *sql.Tx, username, domain, pubkey string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO address_ownership (username, domain, pubkey, valid_from)
		VALUES ($1, $2, $3, NOW())
	`, username, domain, pubkey)
	if err != nil {
		return fmt.Errorf("open ownership: %w", err)
	}
	return nil
}

// closeOwnership sets valid_to=NOW() on all open intervals for (username, domain, pubkey).
func closeOwnership(ctx context.Context, tx *sql.Tx, username, domain, pubkey string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE address_ownership
		SET valid_to = NOW()
		WHERE username = $1 AND domain = $2 AND pubkey = $3 AND valid_to IS NULL
	`, username, domain, pubkey)
	if err != nil {
		return fmt.Errorf("close ownership: %w", err)
	}
	return nil
}

// copyLightningConfig copies the lightning config from fromAddressID to toAddressID
// inside a tx. Used to inherit LN config when granting an alias. No-ops if the
// source has no config or if the target already has one.
func copyLightningConfig(ctx context.Context, tx *sql.Tx, fromAddressID, toAddressID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO address_lightning (
			address_id, mode, proxy_address,
			nwc_relay_url, nwc_wallet_pubkey, nwc_secret_encrypted,
			min_sendable_msats, max_sendable_msats, comment_allowed,
			allows_nostr, nwc_error_count, enabled, updated_at
		)
		SELECT $2, mode, proxy_address,
		       nwc_relay_url, nwc_wallet_pubkey, nwc_secret_encrypted,
		       min_sendable_msats, max_sendable_msats, comment_allowed,
		       allows_nostr, 0, enabled, NOW()
		FROM address_lightning
		WHERE address_id = $1
		ON CONFLICT (address_id) DO NOTHING
	`, fromAddressID, toAddressID)
	if err != nil {
		return fmt.Errorf("copy lightning config: %w", err)
	}
	return nil
}

// Error definitions
var (
	ErrInsufficientCredits = fmt.Errorf("insufficient credits")
)
