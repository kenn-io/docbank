// Package misslog owns sensitive local search receipts. Portable metadata and
// telemetry never consume these records.
package misslog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// Request describes the empty result accepted by a submitting client.
type Request struct {
	Query            string         `json:"query"`
	BindingID        string         `json:"binding_id,omitempty" maxLength:"256"`
	Profile          string         `json:"profile,omitempty" maxLength:"256"`
	RequestedMode    string         `json:"requested_mode" enum:"auto,lexical,semantic,hybrid,fuzzy"`
	ActualMode       string         `json:"actual_mode" enum:"auto,lexical,semantic,hybrid,fuzzy"`
	Filters          map[string]any `json:"filters,omitempty" maxProperties:"32"`
	Coverage         map[string]any `json:"coverage,omitempty" maxProperties:"32"`
	Truncated        bool           `json:"truncated"`
	Degradations     []string       `json:"degradations,omitempty" maxItems:"64"`
	Reranking        map[string]any `json:"reranking,omitempty" maxProperties:"8"`
	FenceFingerprint string         `json:"fence_fingerprint,omitempty" maxLength:"128"`
	DocumentCount    *int           `json:"document_count,omitempty" minimum:"0"`
}

// Receipt adds daemon-owned identity and time to the submitted outcome.
type Receipt struct {
	Request
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	VaultUID  string    `json:"vault_uid"`
}

// Service belongs to one vault owner and one daemon lifetime.
type Service struct {
	store        *store.Store
	config       config.MissLogConfig
	logger       *slog.Logger
	mutate       func(context.Context, func() error) error
	recordMutate func(context.Context, func() error) error
}

// New prunes on enabled startup only. Failures never expose sensitive payloads.
func New(st *store.Store, cfg config.MissLogConfig, logger *slog.Logger, mutators ...func(context.Context, func() error) error) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{store: st, config: cfg, logger: logger, mutate: func(_ context.Context, fn func() error) error { return fn() }}
	if len(mutators) > 0 && mutators[0] != nil {
		s.mutate = mutators[0]
	}
	s.recordMutate = s.mutate
	if len(mutators) > 1 && mutators[1] != nil {
		s.recordMutate = mutators[1]
	}
	if cfg.Enabled && st != nil {
		if err := s.mutate(context.Background(), func() error {
			return st.PruneMissLogRecords(context.Background(), time.Now(), cfg.RetentionDays, cfg.MaxRecords)
		}); err != nil {
			logger.Warn("miss_log_prune_failed")
		}
	}
	return s
}

// Record stores a bounded receipt. Failures leave the search usable.
func (s *Service) Record(ctx context.Context, request Request) {
	if s == nil || !s.config.Enabled || s.store == nil || (strings.TrimSpace(request.Query) == "" && len(request.Filters) == 0) {
		return
	}
	if utf8.RuneCountInString(request.Query) > 32768 || len(request.Profile) > 128 || len(request.BindingID) > 128 {
		s.logger.Warn("miss_log_write_failed")
		return
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.logger.Warn("miss_log_write_failed")
		return
	}
	receipt := Receipt{Request: request, ID: hex.EncodeToString(random[:]), CreatedAt: time.Now().UTC(), VaultUID: s.store.VaultID()}
	encoded, err := json.Marshal(receipt)
	if err != nil || len(encoded) > 64<<10 {
		s.logger.Warn("miss_log_write_failed")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err = s.recordMutate(ctx, func() error {
		return s.store.TryAddMissLogRecord(ctx, store.MissLogRecord{ID: receipt.ID, CreatedAt: receipt.CreatedAt, Receipt: string(encoded)}, receipt.CreatedAt, s.config.RetentionDays, s.config.MaxRecords)
	}); err != nil {
		s.logger.Warn("miss_log_write_failed")
	}
}

// ReceiptScope projects search options or outcomes without large source lists.
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
	delete(scope, "fence")
	delete(scope, "media_sources")
	return scope
}
