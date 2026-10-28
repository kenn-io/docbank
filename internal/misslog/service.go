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
	Coverage         any            `json:"coverage,omitempty" maxProperties:"32"`
	Truncated        bool           `json:"truncated"`
	Degradations     []string       `json:"degradations,omitempty" maxItems:"64"`
	Reranking        any            `json:"reranking,omitempty" maxProperties:"8"`
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
	store  *store.Store
	config config.MissLogConfig
	logger *slog.Logger
	mutate func(context.Context, func() error) error
}

// New prunes on enabled startup only. Failures never expose sensitive payloads.
func New(st *store.Store, cfg config.MissLogConfig, logger *slog.Logger, mutate func(context.Context, func() error) error) *Service {
	s := &Service{store: st, config: cfg, logger: logger, mutate: mutate}
	if cfg.Enabled {
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
	if !s.config.Enabled || (strings.TrimSpace(request.Query) == "" && len(request.Filters) == 0) {
		return
	}
	var random [24]byte
	_, _ = rand.Read(random[:])
	receipt := Receipt{Request: request, ID: hex.EncodeToString(random[:])}
	receipt.CreatedAt = time.Now().UTC()
	receipt.VaultUID = s.store.VaultID()
	encoded, err := json.Marshal(receipt)
	if err != nil {
		s.logger.Warn("miss_log_write_failed")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err = s.mutate(ctx, func() error {
		return s.store.AddMissLogRecord(ctx, store.MissLogRecord{ID: receipt.ID, CreatedAt: receipt.CreatedAt, Receipt: string(encoded)}, receipt.CreatedAt, s.config.RetentionDays, s.config.MaxRecords)
	}); err != nil {
		s.logger.Warn("miss_log_write_failed")
		return
	}
}
