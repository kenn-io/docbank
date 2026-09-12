package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"
	"sync"
	"time"
)

type qualityCacheEntry struct {
	data    []byte
	expires time.Time
	used    uint64
}

// CollectionQualityService caches aggregate receipts, never the authority census.
// Each read checks a fresh snapshot before reusing a receipt.
type CollectionQualityService struct {
	store   *Store
	mu      sync.Mutex
	entries map[string]qualityCacheEntry
	bytes   int
	serial  uint64
	clock   func() time.Time
}

func NewCollectionQualityService(s *Store) *CollectionQualityService {
	return &CollectionQualityService{store: s, entries: make(map[string]qualityCacheEntry), clock: time.Now}
}

func (s *CollectionQualityService) lookup(key string) (CollectionQuality, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return CollectionQuality{}, false
	}
	if !s.clock().Before(entry.expires) {
		delete(s.entries, key)
		s.bytes -= len(entry.data)
		return CollectionQuality{}, false
	}
	var value CollectionQuality
	if json.Unmarshal(entry.data, &value) != nil {
		return CollectionQuality{}, false
	}
	s.serial++
	entry.used = s.serial
	s.entries[key] = entry
	return value, true
}

func (s *CollectionQualityService) remember(key string, value CollectionQuality) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 8<<20 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.entries[key]; ok {
		s.bytes -= len(old.data)
		delete(s.entries, key)
	}
	for len(s.entries) >= 64 || s.bytes+len(data) > 8<<20 {
		var oldest string
		var used = ^uint64(0)
		for candidate, entry := range s.entries {
			if entry.used < used {
				oldest = candidate
				used = entry.used
			}
		}
		s.bytes -= len(s.entries[oldest].data)
		delete(s.entries, oldest)
	}
	s.serial++
	s.entries[key] = qualityCacheEntry{data: data, expires: s.clock().Add(10 * time.Second), used: s.serial}
	s.bytes += len(data)
}

func (s *CollectionQualityService) Read(ctx context.Context, id string, selection CoverageSelection, fields []string) (result CollectionQuality, retErr error) {
	selection, err := normalizeCoverageSelection([]CoverageSelection{selection})
	if err != nil {
		return result, err
	}
	fields, err = normalizeQualityFields(fields)
	if err != nil {
		return result, err
	}
	if validateUUIDv4(id) != nil {
		return result, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, collectionQualityTimeout)
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			result = CollectionQuality{}
			retErr = errors.Join(ErrQualityUnavailable, ctx.Err(), retErr)
		}
	}()
	tx, err := s.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	census, err := collectionQualityCensusTx(ctx, tx, id, selection)
	if err != nil {
		return result, err
	}
	key := strings.Join([]string{id, selection.Configuration, selection.ProfileFingerprint, strings.Join(fields, ","), census.fingerprint}, "\x00")
	result, hit := s.lookup(key)
	if !hit {
		result, err = aggregateCollectionQuality(ctx, census, fields)
		if err != nil {
			return CollectionQuality{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return CollectionQuality{}, err
	}
	if !hit {
		s.remember(key, result)
	}
	return result, nil
}
