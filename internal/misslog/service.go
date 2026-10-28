// Package misslog owns sensitive local search receipts. Portable metadata and
// telemetry never consume these records.
package misslog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"log/slog"
	"time"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// Receipt preserves the search request and outcome without document excerpts.
// Scope contains either the exact version fence or the live GET filters.
type Receipt struct {
	ID               string         `json:"id"`
	CreatedAt        time.Time      `json:"created_at"`
	VaultUID         string         `json:"vault_uid"`
	Query            string         `json:"query"`
	Profile          string         `json:"profile,omitempty"`
	RequestedMode    string         `json:"requested_mode"`
	ActualMode       string         `json:"actual_mode"`
	Scope            map[string]any `json:"scope"`
	Coverage         map[string]any `json:"coverage,omitempty"`
	FenceFingerprint string         `json:"fence_fingerprint,omitempty"`
}

// Service belongs to one vault owner and one daemon lifetime.
type Service struct {
	store  *store.Store
	config config.MissLogConfig
	logger *slog.Logger
	mutate func(context.Context, func() error) error
}

// New prunes on enabled startup only. Failures never expose sensitive payloads.
func New(st *store.Store, cfg config.MissLogConfig, logger *slog.Logger, mutate func(context.Context, func() error) error) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{store: st, config: cfg, logger: logger, mutate: mutate}
	if cfg.Enabled && st != nil {
		if err := s.mutate(context.Background(), func() error {
			return st.PruneMissLogRecords(context.Background(), time.Now(), cfg.RetentionDays, cfg.MaxRecords)
		}); err != nil {
			logger.Warn("miss_log_prune_failed")
		}
	}
	return s
}

// Record persists one successful empty search. An empty ID means logging was
// disabled or failed; the caller's search remains usable in either case.
func (s *Service) Record(ctx context.Context, receipt Receipt) string {
	if s == nil || !s.config.Enabled || s.store == nil {
		return ""
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.logger.Warn("miss_log_write_failed")
		return ""
	}
	receipt.ID = hex.EncodeToString(random[:])
	receipt.CreatedAt = time.Now().UTC()
	receipt.VaultUID = s.store.VaultID()
	encoded, err := json.Marshal(receipt)
	if err != nil || len(encoded) > 8<<20 {
		s.logger.Warn("miss_log_write_failed")
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err = s.mutate(ctx, func() error {
		return s.store.AddMissLogRecord(ctx, store.MissLogRecord{ID: receipt.ID, CreatedAt: receipt.CreatedAt, Receipt: string(encoded)}, receipt.CreatedAt, s.config.RetentionDays, s.config.MaxRecords)
	}); err != nil {
		s.logger.Warn("miss_log_write_failed")
		return ""
	}
	return receipt.ID
}

// ReceiptScope projects a request or coverage object without query text or
// internal recording flags.
func ReceiptScope(input any) map[string]any {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var scope map[string]any
	if json.Unmarshal(encoded, &scope) != nil {
		return nil
	}
	delete(scope, "query")
	delete(scope, "skip_miss_log")
	return scope
}
