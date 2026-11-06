// Package misslog owns sensitive local search receipts. Portable metadata and
// telemetry never consume these records.
package misslog

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/kit/atomicfile"
)

// Request describes the empty result accepted by a submitting client.
type Request struct {
	Query            string         `json:"query" maxLength:"32768"`
	BindingID        string         `json:"binding_id,omitempty" maxLength:"128"`
	Profile          string         `json:"profile,omitempty" maxLength:"128"`
	RequestedMode    string         `json:"requested_mode" enum:"auto,lexical,semantic,hybrid,fuzzy"`
	ActualMode       string         `json:"actual_mode" enum:"auto,lexical,semantic,hybrid,fuzzy"`
	Filters          map[string]any `json:"filters,omitempty"`
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
	mu        sync.Mutex
	dir       string
	vaultUID  string
	files     []string // Oldest first, ordered by creation time and ID.
	config    config.MissLogConfig
	logger    *slog.Logger
	writeFile func(string, []byte, ...atomicfile.Option) error
}

const fileTimeLayout = "20060102T150405.000000000Z"

// New loads the file index and prunes expired and excess receipts.
func New(ctx context.Context, root, vaultUID string, cfg config.MissLogConfig, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{dir: filepath.Join(root, "miss-log"), vaultUID: vaultUID, config: cfg, logger: logger, writeFile: atomicfile.WriteFile}
	entries, err := os.ReadDir(s.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warn("miss_log_load_failed")
		return s
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			s.files = append(s.files, entry.Name())
		}
	}
	slices.Sort(s.files)
	if err := s.prune(ctx, time.Now().UTC()); err != nil {
		logger.Warn("miss_log_prune_failed")
	}
	return s
}

// Record stores a bounded receipt. Failures leave the search usable.
func (s *Service) Record(ctx context.Context, request Request) {
	if s == nil || !s.config.Enabled || (strings.TrimSpace(request.Query) == "" && len(request.Filters) == 0) {
		return
	}
	receipt := Receipt{Request: request, ID: rand.Text(), CreatedAt: time.Now().UTC(), VaultUID: s.vaultUID}
	encoded, err := json.Marshal(receipt)
	if err != nil || len(encoded) > 64<<10 {
		s.logger.Warn("miss_log_write_failed")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		s.logger.Warn("miss_log_write_failed")
		return
	}
	name := receipt.CreatedAt.Format(fileTimeLayout) + "-" + receipt.ID + ".json"
	if err := s.writeFile(filepath.Join(s.dir, name), encoded, atomicfile.WithPrivate()); err != nil {
		s.logger.Warn("miss_log_write_failed")
		if !errors.Is(err, atomicfile.ErrPublished) {
			return
		}
	}
	index, _ := slices.BinarySearch(s.files, name)
	s.files = slices.Insert(s.files, index, name)
	if err := s.prune(ctx, receipt.CreatedAt); err != nil {
		s.logger.Warn("miss_log_prune_failed")
	}
}

func (s *Service) prune(ctx context.Context, now time.Time) error {
	cutoff := now.AddDate(0, 0, -s.config.RetentionDays).Format(fileTimeLayout)
	for len(s.files) > 0 && (len(s.files) > s.config.MaxRecords || s.files[0] < cutoff) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.dir, s.files[0])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		s.files = s.files[1:]
	}
	return nil
}

// List returns visible receipts newest first, including retained records when disabled.
func (s *Service) List(ctx context.Context, limit, offset int) ([]Receipt, error) {
	items := []Receipt{}
	if s == nil {
		return items, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().UTC().AddDate(0, 0, -s.config.RetentionDays).Format(fileTimeLayout)
	for i := len(s.files) - 1; i >= 0 && len(items) < limit; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s.files[i] < cutoff {
			break
		}
		if offset > 0 {
			offset--
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, s.files[i]))
		if err != nil {
			return nil, err
		}
		var receipt Receipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			return nil, err
		}
		items = append(items, receipt)
	}
	return items, nil
}

// Delete removes the receipt whose ID matches the request.
func (s *Service) Delete(ctx context.Context, id string) error {
	if s == nil {
		return fs.ErrNotExist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, name := range s.files {
		if !strings.HasSuffix(name, "-"+id+".json") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
			return err
		}
		s.files = slices.Delete(s.files, i, i+1)
		return nil
	}
	return fs.ErrNotExist
}

// Purge removes all indexed receipts, including expired records.
func (s *Service) Purge(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.files) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.dir, s.files[0])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		s.files = s.files[1:]
	}
	return nil
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
	return scope
}
