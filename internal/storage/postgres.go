package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/lib/pq"

	"git.coldforge.xyz/coldforge/cloistr-me/internal/config"
)

// Storage handles database operations
type Storage struct {
	db *sql.DB
}

// Address represents a user's address record
type Address struct {
	ID              int64
	Username        string
	Domain          string
	Pubkey          string
	Active          bool
	Verified        bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ExpiresAt       *time.Time
	GracePeriodEnds *time.Time
	BanReason       *string
	LastTransferAt  *time.Time
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
	AddressID        int64
	Mode             string // "proxy", "nwc", "disabled"
	ProxyAddress     string
	NWCConnection    string
	MinSendableMsats int64
	MaxSendableMsats int64
	CommentAllowed   int
	AllowsNostr      bool
	Enabled          bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
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

// GetAddressByUsername retrieves an address by username and domain
func (s *Storage) GetAddressByUsername(ctx context.Context, username, domain string) (*Address, error) {
	addr := &Address{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified,
		       created_at, updated_at, expires_at, grace_period_ends, ban_reason
		FROM addresses
		WHERE username = $1 AND domain = $2 AND active = true
	`, username, domain).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.CreatedAt, &addr.UpdatedAt,
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

// GetAddressByPubkey retrieves an address by pubkey
func (s *Storage) GetAddressByPubkey(ctx context.Context, pubkey string) (*Address, error) {
	addr := &Address{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified,
		       created_at, updated_at, expires_at, grace_period_ends, ban_reason
		FROM addresses
		WHERE pubkey = $1 AND active = true
	`, pubkey).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.CreatedAt, &addr.UpdatedAt,
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
	var proxyAddr, nwcConn sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT address_id, mode, proxy_address, nwc_connection,
		       min_sendable_msats, max_sendable_msats, comment_allowed,
		       allows_nostr, enabled, created_at, updated_at
		FROM address_lightning
		WHERE address_id = $1
	`, addressID).Scan(
		&ln.AddressID, &ln.Mode, &proxyAddr, &nwcConn,
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
	return ln, nil
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

// GetAllActiveAddresses retrieves all active addresses for NIP-05 bulk response
func (s *Storage) GetAllActiveAddresses(ctx context.Context, domain string) ([]*Address, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, domain, pubkey, active, verified,
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
			&addr.Active, &addr.Verified, &addr.CreatedAt, &addr.UpdatedAt,
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
func (s *Storage) UpsertLightningConfig(ctx context.Context, addressID int64, mode, proxyAddress string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO address_lightning (address_id, mode, proxy_address, enabled, updated_at)
		VALUES ($1, $2, NULLIF($3, ''), true, NOW())
		ON CONFLICT (address_id) DO UPDATE SET
			mode = EXCLUDED.mode,
			proxy_address = EXCLUDED.proxy_address,
			enabled = CASE WHEN EXCLUDED.mode = 'disabled' THEN false ELSE true END,
			updated_at = NOW()
	`, addressID, mode, proxyAddress)
	if err != nil {
		return fmt.Errorf("failed to upsert lightning config: %w", err)
	}
	return nil
}

// TransferAddress transfers ownership of an address to a new pubkey
func (s *Storage) TransferAddress(ctx context.Context, addressID int64, newPubkey string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE addresses
		SET pubkey = $2,
		    last_transfer_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
	`, addressID, newPubkey)
	if err != nil {
		return fmt.Errorf("failed to transfer address: %w", err)
	}
	return nil
}

// RegisterAddress registers a new address for a pubkey
func (s *Storage) RegisterAddress(ctx context.Context, username, domain, pubkey string) (*Address, error) {
	addr := &Address{}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO addresses (username, domain, pubkey, active, verified, created_at, updated_at)
		VALUES ($1, $2, $3, true, false, NOW(), NOW())
		RETURNING id, username, domain, pubkey, active, verified, created_at, updated_at
	`, username, domain, pubkey).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.CreatedAt, &addr.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to register address: %w", err)
	}
	return addr, nil
}

// AtomicRegisterAddress attempts to register a username atomically
// Returns the address if successful, nil if username was taken
func (s *Storage) AtomicRegisterAddress(ctx context.Context, username, domain, pubkey string) (*Address, error) {
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

	// Register
	addr := &Address{}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO addresses (username, domain, pubkey, active, verified, created_at, updated_at)
		VALUES ($1, $2, $3, true, false, NOW(), NOW())
		RETURNING id, username, domain, pubkey, active, verified, created_at, updated_at
	`, username, domain, pubkey).Scan(
		&addr.ID, &addr.Username, &addr.Domain, &addr.Pubkey,
		&addr.Active, &addr.Verified, &addr.CreatedAt, &addr.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to insert address: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit: %w", err)
	}

	return addr, nil
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

// Error definitions
var (
	ErrInsufficientCredits = fmt.Errorf("insufficient credits")
)
