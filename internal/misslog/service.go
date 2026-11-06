// Package misslog owns sensitive local search receipts. Portable metadata and
// telemetry never consume these records.
package misslog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
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

	Version   int       `json:"version"`
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

const (
	receiptVersion = 1
	fileTimeLayout = "20060102T150405.000000000Z"
)

// New loads the file index and prunes expired and excess receipts.
func New(ctx context.Context, root, vaultUID string, cfg config.MissLogConfig, logger *slog.Logger) *Service {
	defaults := config.Default().MissLog
	if cfg.RetentionDays == 0 {
		cfg.RetentionDays = defaults.RetentionDays
	}
	if cfg.MaxRecords == 0 {
		cfg.MaxRecords = defaults.MaxRecords
	}
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
		if entry.IsDir() || !isReceiptFile(entry.Name()) {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".json") {
			s.files = append(s.files, entry.Name())
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("miss_log_prune_failed")
		}
	}
	slices.Sort(s.files)
	if err := s.prune(ctx, time.Now().UTC()); err != nil {
		logger.Warn("miss_log_prune_failed")
	}
	return s
}

// Record stores a receipt accepted by the HTTP body limit. Failures leave the search usable.
func (s *Service) Record(ctx context.Context, request Request) {
	if s == nil || !s.config.Enabled {
		return
	}
	request.Filters = receiptFilters(request.Filters)
	if strings.TrimSpace(request.Query) == "" && len(request.Filters) == 0 {
		return
	}
	receipt := Receipt{Request: request, Version: receiptVersion, ID: rand.Text(), CreatedAt: time.Now().UTC(), VaultUID: s.vaultUID}
	encoded, err := json.Marshal(receipt)
	if err != nil {
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
	if err := s.prune(context.WithoutCancel(ctx), receipt.CreatedAt); err != nil {
		s.logger.Warn("miss_log_prune_failed")
	}
}

// receiptFilters replaces asset lists with compact identities in flat photo filters and nested QueryV1 filters.
func receiptFilters(filters map[string]any) map[string]any {
	filters = maps.Clone(filters)
	if assetIDs, ok := filters["asset_ids"]; ok {
		encoded, err := json.Marshal(assetIDs)
		var ids []string
		if err == nil && json.Unmarshal(encoded, &ids) == nil && len(ids) > 0 {
			slices.Sort(ids)
			canonical, _ := json.Marshal(ids)
			digest := sha256.Sum256(canonical)
			filters["asset_count"] = len(ids)
			filters["asset_fingerprint"] = "sha256:" + hex.EncodeToString(digest[:])
		}
	}
	delete(filters, "asset_ids")
	if nested, ok := filters["filters"].(map[string]any); ok {
		if nested = receiptFilters(nested); len(nested) > 0 {
			filters["filters"] = nested
		} else {
			delete(filters, "filters")
		}
	}
	return filters
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
		data, err := os.ReadFile(filepath.Join(s.dir, s.files[i]))
		if errors.Is(err, fs.ErrNotExist) {
			s.files = slices.Delete(s.files, i, i+1)
			continue
		}
		if err != nil {
			return nil, err
		}
		var receipt Receipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			return nil, err
		}
		if receipt.Version != receiptVersion {
			s.logger.Warn("miss_log_unsupported_version")
			continue
		}
		if offset > 0 {
			offset--
			continue
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
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		s.files = slices.Delete(s.files, i, i+1)
		return nil
	}
	return fs.ErrNotExist
}

func isReceiptFile(name string) bool {
	return strings.HasSuffix(name, ".json") || strings.Contains(name, ".json.tmp-")
}

// Purge removes all local receipt files, including unfinished writes.
func (s *Service) Purge(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !isReceiptFile(entry.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if i, found := slices.BinarySearch(s.files, entry.Name()); found {
			s.files = slices.Delete(s.files, i, i+1)
		}
	}
	s.files = nil
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
