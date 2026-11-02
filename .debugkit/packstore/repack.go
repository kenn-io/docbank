package packstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"go.kenn.io/kit/pack"
)

const (
	defaultRepackMinAge    = 24 * time.Hour
	defaultRepackDeadBytes = int64(8 << 20)
)

// RepackSelection configures sparse-pack eligibility.
type RepackSelection struct {
	MinAge        time.Duration
	MinDeadStored int64
}

// RepackOptions bounds and dates one reclamation run. MaxBytes greater than
// zero selects bounded automatic behavior that continues past source-content
// failures; zero selects fail-fast explicit behavior.
type RepackOptions struct {
	TargetSize int64
	MaxBytes   int64
	Now        time.Time
	Selection  RepackSelection
}

// RepackStats summarizes committed source-independent reclamation.
type RepackStats struct {
	MappingsPruned         int64
	PacksSelected          int
	PacksRewritten         int
	PacksSealed            int
	PacksRemoved           int
	PacksDeferredOversized int
	BlobsRepacked          int
	BytesRepacked          int64
	BudgetExhausted        bool
}

// Repack retires zero-live packs first, then rewrites each selected sparse
// source independently through an exact catalog compare-and-swap.
func (m *Maintainer) Repack(ctx context.Context, opts RepackOptions) (RepackStats, error) {
	var stats RepackStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if opts.TargetSize < 0 || opts.MaxBytes < 0 || opts.Selection.MinAge < 0 || opts.Selection.MinDeadStored < 0 {
		return stats, ErrInvalidPolicy
	}
	lease, err := m.coordinator.AcquireMaintenance(ctx)
	if err != nil {
		return stats, err
	}
	defer func() { _ = lease.Release() }()
	pruned, err := m.catalog.PruneUnreferenced(ctx)
	if err != nil {
		return stats, err
	}
	stats.MappingsPruned = pruned
	usage, err := m.catalog.ListPackUsage(ctx)
	if err != nil {
		return stats, err
	}
	selected := selectRepackSources(usage, opts)
	stats.PacksSelected = len(selected)
	automatic := opts.MaxBytes > 0
	var runErr error
	var partial []PackUsage
	for _, source := range selected {
		if source.LiveEntries != 0 {
			partial = append(partial, source)
			continue
		}
		if err := m.retireEmpty(ctx, source.PackID, &stats); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	for _, source := range partial {
		if err := ctx.Err(); err != nil {
			return stats, errors.Join(runErr, err)
		}
		if automatic && stats.BytesRepacked >= opts.MaxBytes {
			stats.BudgetExhausted = true
			break
		}
		if source.MaxLiveRawLen > m.limits.BlobBytes || source.MaxLiveStoredLen > m.limits.BlobBytes {
			stats.PacksDeferredOversized++
			continue
		}
		entries, err := m.catalog.ListLivePackEntries(ctx, source.PackID)
		if err != nil {
			return stats, errors.Join(runErr, err)
		}
		if err := verifyUsage(source, entries); err != nil {
			return stats, errors.Join(runErr, err)
		}
		if err := m.preflightRepackSource(source.PackID, entries); err != nil {
			err = fmt.Errorf("packstore: repack source %s: %w", source.PackID, err)
			if errors.Is(err, ErrBlobTooLarge) {
				stats.PacksDeferredOversized++
				continue
			}
			if automatic && isSourceContentError(err) {
				runErr = errors.Join(runErr, err)
				continue
			}
			return stats, errors.Join(runErr, err)
		}
		result, err := m.rewriteSource(ctx, source.PackID, entries, opts.TargetSize)
		if err != nil {
			err = fmt.Errorf("packstore: repack source %s: %w", source.PackID, err)
			if errors.Is(err, ErrBlobTooLarge) {
				stats.PacksDeferredOversized++
				continue
			}
			if automatic && isSourceContentError(err) {
				runErr = errors.Join(runErr, err)
				continue
			}
			return stats, errors.Join(runErr, err)
		}
		if err := m.catalog.CommitRepack(ctx, []string{source.PackID}, result.records, result.moves); err != nil {
			return stats, errors.Join(runErr, err)
		}
		stats.PacksRewritten++
		stats.PacksSealed += len(result.records)
		stats.BlobsRepacked += len(result.moves)
		stats.BytesRepacked += result.rawBytes
		if err := m.retireEmpty(ctx, source.PackID, &stats); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	return stats, runErr
}

func selectRepackSources(usage []PackUsage, opts RepackOptions) []PackUsage {
	now := opts.Now.UTC()
	if opts.Now.IsZero() {
		now = time.Now().UTC()
	}
	minAge := opts.Selection.MinAge
	if minAge == 0 {
		minAge = defaultRepackMinAge
	}
	minDead := opts.Selection.MinDeadStored
	if minDead == 0 {
		minDead = defaultRepackDeadBytes
	}
	ordered := append([]PackUsage(nil), usage...)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].PackID < ordered[j].PackID
	})
	var zero, partial []PackUsage
	for _, source := range ordered {
		if source.LiveEntries == 0 {
			zero = append(zero, source)
			continue
		}
		deadStored := source.StoredBytes - source.LiveStoredBytes
		belowHalf := source.EntryCount > 0 && source.LiveEntries <= (source.EntryCount-1)/2
		oldEnough := !source.CreatedAt.After(now.Add(-minAge))
		if belowHalf && oldEnough && deadStored >= minDead {
			partial = append(partial, source)
		}
	}
	return append(zero, partial...)
}

func verifyUsage(source PackUsage, entries []IndexEntry) error {
	var stored, raw int64
	for _, entry := range entries {
		stored += entry.StoredLen
		raw += entry.RawLen
	}
	if int64(len(entries)) != source.LiveEntries || stored != source.LiveStoredBytes || raw != source.LiveRawBytes {
		return fmt.Errorf("packstore: source %s live inventory changed during selection", source.PackID)
	}
	return nil
}

func (m *Maintainer) preflightRepackSource(packID string, entries []IndexEntry) error {
	reader, err := OpenMaintenancePack(m.layout.PackPath(packID), m.limits)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	footer := make(map[Hash]pack.Entry)
	for _, entry := range reader.Entries() {
		hash, parseErr := ParseHash(entry.ID.String())
		if parseErr != nil {
			return parseErr
		}
		footer[hash] = entry
	}
	for _, indexed := range entries {
		authoritative, ok := footer[indexed.Hash]
		if !ok || !packIndexMatchesFooter(&indexed, authoritative) {
			return fmt.Errorf("%w: source %s metadata mismatch for %s", pack.ErrCorrupt, packID, indexed.Hash)
		}
		if authoritative.RawLen > uint64(m.limits.BlobBytes) {
			return newLimitError(LimitBlobRawBytes, authoritative.RawLen, uint64(m.limits.BlobBytes))
		}
		if authoritative.StoredLen > uint64(m.limits.BlobBytes) {
			return newLimitError(LimitBlobStoredBytes, authoritative.StoredLen, uint64(m.limits.BlobBytes))
		}
	}
	return nil
}

type rewriteResult struct {
	records  []PackRecord
	moves    []RepackMove
	rawBytes int64
}

type rewriteSourceEntry struct {
	oldPackID string
	indexed   IndexEntry
	sealed    pack.Entry
}

func (m *Maintainer) rewriteSource(ctx context.Context, oldPackID string, entries []IndexEntry, targetSize int64) (rewriteResult, error) {
	var result rewriteResult
	var writer *pack.Writer
	var current []rewriteSourceEntry
	abort := func() {
		if writer != nil {
			_ = writer.Abort()
		}
	}
	defer abort()
	seal := func() error {
		if writer == nil {
			return nil
		}
		packID := writer.ID()
		path := m.layout.PackPath(packID)
		sealed, err := writer.Seal(path)
		if err != nil {
			return err
		}
		if err := validateSealedOutput(path, m.limits); err != nil {
			return err
		}
		if len(sealed) != len(current) {
			return errors.New("packstore: replacement entry count changed")
		}
		record := PackRecord{PackID: packID, EntryCount: int64(len(sealed)), CreatedAt: time.Now().UTC()}
		for i, entry := range sealed {
			if entry.ID.String() != current[i].indexed.Hash.String() {
				return fmt.Errorf("%w: replacement hash mismatch", ErrContentMismatch)
			}
			newEntry := indexFromPack(packID, entry)
			record.StoredBytes += newEntry.StoredLen
			result.moves = append(result.moves, RepackMove{OldPackID: oldPackID, NewEntry: newEntry})
		}
		result.records = append(result.records, record)
		writer = nil
		current = nil
		return nil
	}
	for _, indexed := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		stream, size, err := m.store.OpenStream(ctx, indexed.Hash)
		if err != nil {
			return result, fmt.Errorf("packstore: read source %s blob %s: %w", oldPackID, indexed.Hash, err)
		}
		if size != indexed.RawLen {
			_ = stream.Close()
			return result, fmt.Errorf("%w: source length mismatch", ErrContentMismatch)
		}
		id, err := pack.ParseBlobID(indexed.Hash.String())
		if err != nil {
			_ = stream.Close()
			return result, err
		}
		prepared, prepareErr := pack.PrepareBlob(ctx, stream, uint64(size), pack.DefaultZstdLevel, pack.AppendStreamOptions{
			ExpectedID: &id, ScratchDir: m.layout.PacksDir(),
		})
		streamErr := stream.Close()
		if err := errors.Join(prepareErr, streamErr); err != nil {
			if prepared != nil {
				_ = prepared.Close()
			}
			return result, fmt.Errorf("packstore: read source %s blob %s: %w", oldPackID, indexed.Hash, err)
		}
		if prepared == nil {
			return result, fmt.Errorf("packstore: prepare source %s blob %s returned no result", oldPackID, indexed.Hash)
		}
		if err := checkPlainOutput(m.limits, uint64(pack.MinEntryOffset), prepared.StoredLen(), 1); err != nil {
			_ = prepared.Close()
			return result, err
		}
		if writer != nil {
			if err := checkPlainOutput(m.limits, uint64(writer.StoredSize()), prepared.StoredLen(), len(current)+1); err != nil {
				if err := seal(); err != nil {
					_ = prepared.Close()
					return result, err
				}
			}
		}
		if writer == nil {
			writer, err = pack.NewWriter(m.layout.PacksDir(), pack.WriterOptions{TargetSize: targetSize})
			if err != nil {
				_ = prepared.Close()
				return result, err
			}
		}
		sealed, err := writer.AppendPrepared(ctx, prepared)
		if err != nil {
			return result, err
		}
		current = append(current, rewriteSourceEntry{oldPackID: oldPackID, indexed: indexed, sealed: sealed})
		result.rawBytes += size
		if writer.Full() {
			if err := seal(); err != nil {
				return result, err
			}
		}
	}
	return result, seal()
}

func (m *Maintainer) retireEmpty(ctx context.Context, packID string, stats *RepackStats) error {
	deleted, err := m.catalog.DeleteEmptyPackRecord(ctx, packID)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("packstore: pack %s still has mappings", packID)
	}
	if err := m.filesystem.Retire(ctx, ObjectRef{PackID: packID}); err != nil {
		return err
	}
	stats.PacksRemoved++
	return nil
}

func isSourceContentError(err error) bool {
	for _, known := range []error{
		fs.ErrNotExist, pack.ErrBadMagic, pack.ErrUnsupportedVersion,
		pack.ErrTruncated, pack.ErrChecksum, pack.ErrCorrupt, pack.ErrBlobMismatch, ErrContentMismatch,
	} {
		if errors.Is(err, known) {
			return true
		}
	}
	pathErr, ok := errors.AsType[*os.PathError](err)
	return ok && errors.Is(pathErr, fs.ErrNotExist)
}
