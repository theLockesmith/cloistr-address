package storage

import (
	"context"
	"fmt"
)

// EffectiveQuotaResult is the resolved quota for a pubkey + quota type: the
// identity-scaled limit (anonymous vs named vs per-user override) plus any
// non-expired grants, and the SUM of per-service usage. Limit 0 = unlimited.
type EffectiveQuotaResult struct {
	Limit     int64 `json:"limit"`
	Used      int64 `json:"used"`
	Remaining int64 `json:"remaining"`
}

// EffectiveQuota returns the authoritative quota for a pubkey + quota type via the
// effective_quota() DB function (see cloistr-me migration 007).
func (s *Storage) EffectiveQuota(ctx context.Context, pubkey, quotaType string) (EffectiveQuotaResult, error) {
	var r EffectiveQuotaResult
	err := s.db.QueryRowContext(ctx,
		`SELECT quota_limit, current_usage, remaining FROM effective_quota($1, $2)`,
		pubkey, quotaType,
	).Scan(&r.Limit, &r.Used, &r.Remaining)
	if err != nil {
		return EffectiveQuotaResult{}, fmt.Errorf("effective_quota: %w", err)
	}
	return r, nil
}

// CheckQuota reports whether pubkey can absorb additionalBytes more of quotaType,
// via the check_quota() DB function. Unlimited quotas always pass.
func (s *Storage) CheckQuota(ctx context.Context, pubkey, quotaType string, additionalBytes int64) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT check_quota($1, $2, $3)`,
		pubkey, quotaType, additionalBytes,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check_quota: %w", err)
	}
	return ok, nil
}

// RecordServiceUsage adds deltaBytes to this (pubkey, quotaType, service) usage
// component in user_quota_usage. The delta is additive and clamped at 0 so a
// release (negative delta) never drives a component negative — matching the
// platform library's RecordUsage semantics. Services that maintain their own
// authoritative totals should instead have the reconciler REPLACE their component.
func (s *Storage) RecordServiceUsage(ctx context.Context, pubkey, quotaType, service string, deltaBytes int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_quota_usage (pubkey, quota_type_id, service, bytes, updated_at)
		VALUES ($1, $2, $3, GREATEST(0, $4::BIGINT), NOW())
		ON CONFLICT (pubkey, quota_type_id, service) DO UPDATE
		SET bytes = GREATEST(0, user_quota_usage.bytes + $4::BIGINT),
		    updated_at = NOW()
	`, pubkey, quotaType, service, deltaBytes)
	if err != nil {
		return fmt.Errorf("record service usage: %w", err)
	}
	return nil
}
