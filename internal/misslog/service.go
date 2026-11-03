// Package misslog owns sensitive local search receipts. Portable metadata and
// telemetry never consume these records.
package misslog

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// Receipt preserves the search request and outcome without document excerpts.
// Scope contains either the exact version fence or the live GET filters.
type Receipt struct {
	Source           string         `json:"source"`
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
	Triggers         []string       `json:"triggers"`
	Hits             []Hit          `json:"hits,omitempty"`
	Feedback         *Feedback      `json:"feedback,omitempty"`
}

// Service belongs to one vault owner and one daemon (or embedded vault) lifetime.
type Service struct {
	mu           sync.Mutex
	secret       [32]byte
	pending      []pendingReceipt
	pendingBytes int
	now          func() time.Time
	store        *store.Store
	config       config.MissLogConfig
	logger       *slog.Logger
	mutate       func(context.Context, func() error) error
	recordMutate func(context.Context, func() error) error
	deleteMutate func(context.Context, func() error) error
}

// New prunes on enabled startup only. The first mutator admits explicit writes;
// the optional second admits automatic recording without waiting for
// maintenance, and the third admits deletion only when maintenance is idle.
// Failures never expose sensitive payloads.
func New(st *store.Store, cfg config.MissLogConfig, logger *slog.Logger, mutators ...func(context.Context, func() error) error) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{now: time.Now, store: st, config: cfg, logger: logger, mutate: func(_ context.Context, fn func() error) error { return fn() }}
	if _, err := rand.Read(s.secret[:]); err != nil {
		panic("miss log entropy unavailable")
	}
	if len(mutators) > 0 && mutators[0] != nil {
		s.mutate = mutators[0]
	}
	s.recordMutate = s.mutate
	if len(mutators) > 1 && mutators[1] != nil {
		s.recordMutate = mutators[1]
	}
	s.deleteMutate = s.mutate
	if len(mutators) > 2 && mutators[2] != nil {
		s.deleteMutate = mutators[2]
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

// Record preserves the empty-search entry point for callers without feedback.
func (s *Service) Record(ctx context.Context, receipt Receipt) string {
	return s.Observe(ctx, receipt).ID
}

// Ticket authorizes bounded feedback without exposing any stored query.
// Capability is derived from this service incarnation and is never persisted.
type Ticket struct {
	ID         string
	Capability string
}

type pendingReceipt struct {
	id        string
	createdAt time.Time
	encoded   []byte
}

// Observe persists empty searches and caches successful searches with hits.
// An empty ticket means disabled, oversized, or failed logging.
func (s *Service) Observe(ctx context.Context, receipt Receipt) Ticket {
	if s == nil || !s.config.Enabled || s.store == nil {
		return Ticket{}
	}
	if utf8.RuneCountInString(receipt.Query) > 32768 || len(receipt.Profile) > 128 || !validHits(receipt.Hits) {
		s.logger.Warn("miss_log_write_failed")
		return Ticket{}
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.logger.Warn("miss_log_write_failed")
		return Ticket{}
	}
	if receipt.Source == "" {
		receipt.Source = "daemon"
	}
	receipt.ID = hex.EncodeToString(random[:])
	receipt.CreatedAt = s.now().UTC()
	receipt.VaultUID = s.store.VaultID()
	receipt.Feedback = nil
	receipt.Triggers = []string{}
	if len(receipt.Hits) == 0 {
		receipt.Triggers = []string{"no_match"}
	}
	encoded, err := json.Marshal(receipt)
	if err != nil || len(encoded) > maxPendingBytes {
		s.logger.Warn("miss_log_write_failed")
		return Ticket{}
	}
	err = s.recordMutate(ctx, func() error {
		if !s.mu.TryLock() {
			return ErrPersistence
		}
		defer s.mu.Unlock()
		s.expirePending()
		if len(receipt.Hits) == 0 {
			return s.store.TryAddMissLogRecord(ctx, store.MissLogRecord{ID: receipt.ID, CreatedAt: receipt.CreatedAt, Receipt: string(encoded)}, s.now(), s.config.RetentionDays, s.config.MaxRecords)
		}
		// Retain only owned encoded bytes, avoiding a second copy of the query.
		for len(s.pending) >= 50 || s.pendingBytes+len(encoded) > maxPendingBytes {
			s.removePending(0)
		}
		s.pending = append(s.pending, pendingReceipt{id: receipt.ID, createdAt: receipt.CreatedAt, encoded: encoded})
		s.pendingBytes += len(encoded)
		return nil
	})
	if err != nil {
		s.logger.Warn("miss_log_write_failed")
		return Ticket{}
	}
	return Ticket{ID: receipt.ID, Capability: s.capability(receipt.ID)}
}

func (s *Service) capability(id string) string {
	mac := hmac.New(sha256.New, s.secret[:])
	mac.Write([]byte(s.store.VaultID()))
	mac.Write([]byte{0})
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Service) removePending(i int) {
	s.pendingBytes -= len(s.pending[i].encoded)
	copy(s.pending[i:], s.pending[i+1:])
	s.pending[len(s.pending)-1] = pendingReceipt{}
	s.pending = s.pending[:len(s.pending)-1]
}
func (s *Service) expirePending() {
	for i := len(s.pending) - 1; i >= 0; i-- {
		if !s.pending[i].createdAt.Add(15 * time.Minute).After(s.now()) {
			s.removePending(i)
		}
	}
}

// Enabled reports this service's configured recording state. A nil service is disabled.
func (s *Service) Enabled() bool { return s != nil && s.config.Enabled }
