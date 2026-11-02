package packstore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
)

// PackOptions bounds one packing run. MaxBytes is a soft committed raw-byte
// budget; zero is unlimited.
type PackOptions struct {
	TargetSize int64
	MaxBytes   int64
	// MaxVerifyBytes bounds existing packed-content checks by the larger of
	// raw and stored bytes, allowing one entry to exceed the remaining budget.
	// Zero checks the full inventory. Positive values resume across calls on
	// this Maintainer, up to the last entry present when the sweep started.
	// A new Maintainer starts a fresh sweep.
	MaxVerifyBytes int64
}

// PackStats summarizes committed and repair outcomes.
type PackStats struct {
	BlobsChecked int
	// VerificationPending reports an unfinished sweep. Entries beyond its
	// captured end wait for the next one.
	VerificationPending        bool
	PacksSealed                int
	BlobsPacked                int
	BytesPacked                int64
	PacksAdopted               int
	PacksRemoved               int
	PacksQuarantined           int
	PacksUnreadable            int
	RecordsDropped             int
	MappingsPruned             int64
	BlobsMissing               int
	BlobsCorrupt               int
	BlobsDeferredOversized     int
	PacksDeferredOversized     int
	LooseSwept                 int
	LooseOrphansRemoved        int
	LooseOrphanSweepSuppressed bool
	BudgetExhausted            bool
}

// Pack repairs inventory, reconciles orphan packs, packs loose members, and
// sweeps verified redundant loose files in that order.
func (m *Maintainer) Pack(ctx context.Context, opts PackOptions) (PackStats, error) {
	var stats PackStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if opts.TargetSize < 0 || opts.MaxBytes < 0 || opts.MaxVerifyBytes < 0 {
		return stats, ErrInvalidPolicy
	}
	lease, err := m.coordinator.AcquireMaintenance(ctx)
	if err != nil {
		return stats, err
	}
	defer func() { _ = lease.Release() }()
	// A failed pass must revisit any repair candidates that were not committed.
	previous, previousEnd := m.verifiedThrough, m.verificationEnd
	completed := false
	defer func() {
		if !completed {
			m.verifiedThrough, m.verificationEnd = previous, previousEnd
		}
	}()
	if err := pack.MkdirAllSynced(m.layout.PacksDir()); err != nil {
		return stats, err
	}
	if err := m.cleanPackStaging(ctx); err != nil {
		return stats, err
	}
	inventory, err := m.catalog.ListReferences(ctx)
	if err != nil {
		return stats, err
	}
	refs := make(map[Hash]Reference, len(inventory.References))
	for _, reference := range inventory.References {
		refs[reference.Hash] = reference
	}
	recoveries, err := m.dropDangling(ctx, &stats, refs, opts.MaxVerifyBytes)
	if err != nil {
		return stats, err
	}
	if err := m.packCandidates(ctx, recoveries, opts, &stats, packCandidatePolicy{
		commit:          m.catalog.AdoptPack,
		openPin:         m.openVerificationPin,
		replaceMappings: true,
	}); err != nil {
		return stats, err
	}
	// A packing budget can defer some verified repair candidates as well.
	if stats.BudgetExhausted {
		m.verifiedThrough, m.verificationEnd = previous, previousEnd
	}
	pruned, err := m.catalog.PruneUnreferenced(ctx)
	if err != nil {
		return stats, err
	}
	stats.MappingsPruned += pruned
	if inventory.Complete {
		if err := m.reconcileOrphans(ctx, refs, &stats); err != nil {
			return stats, err
		}
	}
	if !stats.BudgetExhausted {
		if err := m.packLoose(ctx, opts, &stats); err != nil {
			return stats, err
		}
	}
	if err := m.sweepLoose(ctx, refs, inventory.Complete, &stats); err != nil {
		return stats, err
	}
	completed = true
	return stats, nil
}

func (m *Maintainer) cleanPackStaging(ctx context.Context) error {
	entries, err := os.ReadDir(m.layout.PacksDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".staging") {
			if err := os.Remove(filepath.Join(m.layout.PacksDir(), entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Maintainer) dropDangling(
	ctx context.Context, stats *PackStats, refs map[Hash]Reference, maxVerifyBytes int64,
) ([]Candidate, error) {
	records, err := m.catalog.ListPackRecords(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := os.Stat(m.layout.PackPath(record.PackID)); errors.Is(err, fs.ErrNotExist) {
			if err := m.catalog.DeletePackRecord(ctx, record.PackID); err != nil {
				return nil, err
			}
			stats.RecordsDropped++
		} else if err != nil {
			return nil, err
		}
	}
	indexed, err := m.catalog.ListIndexed(ctx)
	if err != nil {
		return nil, err
	}
	// Physical ordering avoids reopening packs for hash-interleaved entries.
	slices.SortFunc(indexed, compareVerificationEntry)
	if maxVerifyBytes > 0 && m.verificationEnd == (IndexEntry{}) && len(indexed) > 0 {
		// New packs must not keep a sweep from returning to older entries.
		m.verificationEnd = indexed[len(indexed)-1]
	}
	var recoveries []Candidate
	remaining := maxVerifyBytes
	for _, entry := range indexed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if maxVerifyBytes > 0 {
			if compareVerificationEntry(entry, m.verificationEnd) > 0 {
				break
			}
			if compareVerificationEntry(entry, m.verifiedThrough) <= 0 {
				continue
			}
		}
		if maxVerifyBytes > 0 && remaining == 0 {
			stats.VerificationPending = true
			return recoveries, nil
		}
		recovery, err := m.repairIndexed(ctx, stats, refs, entry)
		if err != nil {
			return nil, err
		}
		if recovery != nil {
			recoveries = append(recoveries, *recovery)
		}
		stats.BlobsChecked++
		if maxVerifyBytes > 0 {
			remaining = max(0, remaining-max(1, entry.RawLen, entry.StoredLen))
			m.verifiedThrough = entry
		}
	}
	m.verifiedThrough = IndexEntry{}
	m.verificationEnd = IndexEntry{}
	return recoveries, nil
}

func compareVerificationEntry(a, b IndexEntry) int {
	return cmp.Or(cmp.Compare(a.PackID, b.PackID), cmp.Compare(a.Offset, b.Offset), cmp.Compare(a.Hash, b.Hash))
}

func (m *Maintainer) repairIndexed(ctx context.Context, stats *PackStats, refs map[Hash]Reference, entry IndexEntry) (*Candidate, error) {
	if _, err := os.Stat(m.layout.PackPath(entry.PackID)); errors.Is(err, fs.ErrNotExist) {
		if err := m.catalog.DeleteIndexEntry(ctx, entry.Hash); err != nil {
			return nil, err
		}
		stats.MappingsPruned++
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	stream, _, err := m.store.openPackedStream(ctx, entry.Hash, &entry)
	if err == nil {
		_, err = io.Copy(io.Discard, stream)
		err = errors.Join(err, stream.Close())
	}
	if err == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logicalSize, valid, err := m.findAnyValidCanonicalLoose(ctx, entry.Hash)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, nil
	}
	reference, live := refs[entry.Hash]
	if !live {
		return nil, nil
	}
	// Keep the unreadable mapping authoritative until the verified replacement
	// is sealed and AdoptPack atomically advances the catalog.
	return &Candidate{
		Hash: entry.Hash, OriginalHashes: append([]string(nil), reference.OriginalHashes...),
		Paths: []string{m.layout.CompressedLoosePath(entry.Hash), m.layout.LoosePath(entry.Hash)}, Size: logicalSize,
	}, nil
}

func (m *Maintainer) reconcileOrphans(ctx context.Context, refs map[Hash]Reference, stats *PackStats) error {
	return filepath.WalkDir(m.layout.PacksDir(), func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), PackExt) {
			return nil
		}
		packID := strings.TrimSuffix(entry.Name(), PackExt)
		if !pack.IsValidPackID(packID) || filepath.Clean(path) != filepath.Clean(m.layout.PackPath(packID)) {
			return nil
		}
		hasRecord, err := m.catalog.HasPackRecord(ctx, packID)
		if err != nil || hasRecord {
			return err
		}
		return m.reconcileOne(ctx, path, packID, refs, stats)
	})
}

func (m *Maintainer) reconcileOne(ctx context.Context, path, packID string, refs map[Hash]Reference, stats *PackStats) error {
	reader, err := OpenMaintenancePack(path, m.limits)
	if err != nil {
		if errors.Is(err, ErrBlobTooLarge) {
			stats.PacksDeferredOversized++
			return nil
		}
		stats.PacksUnreadable++
		return nil
	}
	readerOpen := true
	defer func() {
		if readerOpen {
			_ = reader.Close()
		}
	}()
	footer := reader.Entries()
	record := PackRecord{PackID: packID, EntryCount: int64(len(footer)), CreatedAt: time.Now().UTC()}
	adoptions := make([]Adoption, 0, len(footer))
	for _, footerEntry := range footer {
		if err := ctx.Err(); err != nil {
			return err
		}
		record.StoredBytes += int64(footerEntry.StoredLen)
		hash, err := ParseHash(footerEntry.ID.String())
		if err != nil {
			return err
		}
		reference, live := refs[hash]
		if !live {
			continue
		}
		if footerEntry.RawLen > uint64(m.limits.BlobBytes) || footerEntry.StoredLen > uint64(m.limits.BlobBytes) {
			stats.PacksDeferredOversized++
			return nil
		}
		location, err := m.catalog.Resolve(ctx, hash)
		if err != nil {
			return err
		}
		if location.Pack != nil && location.Pack.PackID != packID {
			if _, _, readErr := m.store.ReadBounded(ctx, hash, m.limits.BlobBytes); readErr == nil {
				continue
			}
		}
		if location.Member && location.Pack == nil {
			valid, verifyErr := m.hasAuthoritativeCanonicalLoose(ctx, hash)
			if verifyErr != nil {
				return verifyErr
			}
			if valid {
				continue
			}
		}
		stream, err := reader.OpenBlob(ctx, hash)
		if err != nil {
			stats.PacksQuarantined++
			return nil
		}
		if err := errors.Join(stream.Verify(), stream.Close()); err != nil {
			stats.PacksQuarantined++
			return nil
		}
		adoptions = append(adoptions, Adoption{Entry: indexFromPack(packID, footerEntry), OriginalHashes: reference.OriginalHashes})
	}
	if err := reader.Close(); err != nil {
		return err
	}
	readerOpen = false
	if len(adoptions) == 0 {
		if err := os.Remove(path); err != nil {
			return err
		}
		stats.PacksRemoved++
		return nil
	}
	if err := m.catalog.AdoptPack(ctx, record, adoptions); err != nil {
		return err
	}
	stats.PacksAdopted++
	return nil
}

type packedSource struct {
	candidate Candidate
	path      string
	pin       identityPin
	entry     pack.Entry
}

type identityPin interface {
	Stat() (fs.FileInfo, error)
	Close() error
	removeClaim(string) error
}

type verificationOnlyIdentityPin struct {
	looseVerificationIdentityPin
}

func (*verificationOnlyIdentityPin) removeClaim(string) error {
	return fmt.Errorf("packstore: verification-only identity pin cannot remove content: %w", errors.ErrUnsupported)
}

type identityPinOpener func(string) (identityPin, fs.FileInfo, error)

var removePinnedLooseClaim = func(pin identityPin, path string) error {
	return pin.removeClaim(path)
}

// openLooseIdentityPin linearizes namespace-only removal at pin acquisition.
// Callers that verified an earlier descriptor must separately compare its
// identity with the returned pin before authorizing content cleanup.
func openLooseIdentityPin(path string) (identityPin, fs.FileInfo, error) {
	pin, pinned, err := openLooseRemovalIdentityPin(path)
	if err != nil {
		if current, snapshotErr := snapshotPathIdentity(path); snapshotErr == nil {
			if validationErr := validateRegularNoFollow(path, current); validationErr != nil {
				return nil, nil, errors.Join(validationErr, err)
			}
		}
		return nil, nil, err
	}
	if pin == nil || pinned == nil {
		return nil, nil, errors.Join(fmt.Errorf("packstore: identity pin for %s has incomplete state", path), closeIdentityPin(pin))
	}
	if err := validateRegularNoFollow(path, pinned); err != nil {
		return nil, nil, errors.Join(err, pin.Close())
	}
	return pin, pinned, nil
}

func openLooseMaintenanceVerificationPin(path string) (identityPin, fs.FileInfo, error) {
	pin, identity, err := openLooseVerificationIdentityPin(path)
	if err != nil {
		return nil, nil, err
	}
	return &verificationOnlyIdentityPin{looseVerificationIdentityPin: pin}, identity, nil
}

type candidateGroup struct {
	Candidate
	err error
}

func mergeLogicalCandidates(candidates []Candidate) []candidateGroup {
	groups := make([]candidateGroup, 0, len(candidates))
	byHash := make(map[Hash]int, len(candidates))
	pathSets := make([]map[string]struct{}, 0, len(candidates))
	aliasSets := make([]map[string]struct{}, 0, len(candidates))
	for _, candidate := range candidates {
		index, exists := byHash[candidate.Hash]
		if !exists {
			index = len(groups)
			byHash[candidate.Hash] = index
			groups = append(groups, candidateGroup{
				Hash: candidate.Hash,
				Size: candidate.Size,
			})
			pathSets = append(pathSets, make(map[string]struct{}, len(candidate.Paths)))
			aliasSets = append(aliasSets, make(map[string]struct{}, len(candidate.OriginalHashes)))
		} else if groups[index].Size != candidate.Size {
			groups[index].err = errors.Join(groups[index].err, fmt.Errorf(
				"%w: candidate %s has contradictory logical sizes %d and %d",
				ErrContentMismatch, candidate.Hash, groups[index].Size, candidate.Size,
			))
		}
		for _, path := range candidate.Paths {
			if _, duplicate := pathSets[index][path]; duplicate {
				continue
			}
			pathSets[index][path] = struct{}{}
			groups[index].Paths = append(groups[index].Paths, path)
		}
		for _, alias := range candidate.OriginalHashes {
			if _, duplicate := aliasSets[index][alias]; duplicate {
				continue
			}
			aliasSets[index][alias] = struct{}{}
			groups[index].OriginalHashes = append(groups[index].OriginalHashes, alias)
		}
	}
	return groups
}

func (m *Maintainer) packLoose(ctx context.Context, opts PackOptions, stats *PackStats) (resultErr error) {
	candidates, err := m.catalog.ListUnpacked(ctx)
	if err != nil {
		return err
	}
	return m.packCandidates(ctx, candidates, opts, stats, packCandidatePolicy{
		commit:       m.catalog.RecordPack,
		openPin:      m.openVerificationPin,
		removeSource: true,
	})
}

type packCatalogCommit func(context.Context, PackRecord, []Adoption) error

type packCandidatePolicy struct {
	commit          packCatalogCommit
	openPin         identityPinOpener
	replaceMappings bool
	removeSource    bool
}

func (m *Maintainer) packCandidates(
	ctx context.Context,
	candidates []Candidate,
	opts PackOptions,
	stats *PackStats,
	policy packCandidatePolicy,
) (resultErr error) {
	var writer *pack.Writer
	var sources []packedSource
	rawBytes := stats.BytesPacked
	abort := func() error {
		if writer != nil {
			return writer.Abort()
		}
		return nil
	}
	defer func() {
		resultErr = errors.Join(resultErr, abort(), closePackedSourcePins(sources))
	}()
	seal := func() error {
		if writer == nil || len(sources) == 0 {
			return nil
		}
		sealedSources := sources
		sources = nil
		if err := m.sealAndCommit(ctx, writer, sealedSources, stats, policy); err != nil {
			return err
		}
		writer = nil
		return nil
	}
	for _, group := range mergeLogicalCandidates(candidates) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if group.err != nil {
			stats.BlobsCorrupt++
			continue
		}
		candidate := group.Candidate
		if candidate.Size > m.limits.BlobBytes {
			stats.BlobsDeferredOversized++
			continue
		}
		if writer != nil && len(sources) >= m.packedSourcePinLimit {
			if err := seal(); err != nil {
				return err
			}
		}
		prepared, source, sourcePin, found, readErr := m.prepareCandidate(ctx, candidate, policy.openPin)
		if readErr != nil {
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				return readErr
			}
			if errors.Is(readErr, ErrBlobTooLarge) {
				stats.BlobsDeferredOversized++
				continue
			}
			stats.BlobsCorrupt++
			continue
		}
		if !found {
			stats.BlobsMissing++
			continue
		}
		if prepared == nil || sourcePin == nil {
			var closePreparedErr error
			if prepared != nil {
				closePreparedErr = prepared.Close()
			}
			return errors.Join(
				fmt.Errorf("packstore: prepared candidate %s has incomplete state", candidate.Hash),
				closePreparedErr,
				closeIdentityPin(sourcePin),
			)
		}
		if err := checkPlainOutput(m.limits, uint64(pack.MinEntryOffset), prepared.StoredLen(), 1); err != nil {
			if closeErr := errors.Join(prepared.Close(), sourcePin.Close()); closeErr != nil {
				return closeErr
			}
			stats.BlobsDeferredOversized++
			continue
		}
		if writer != nil {
			if err := checkPlainOutput(m.limits, uint64(writer.StoredSize()), prepared.StoredLen(), len(sources)+1); err != nil {
				if err := seal(); err != nil {
					return errors.Join(err, prepared.Close(), sourcePin.Close())
				}
			}
		}
		if writer == nil {
			newWriter, err := pack.NewWriter(m.layout.PacksDir(), pack.WriterOptions{TargetSize: opts.TargetSize})
			if err != nil {
				return errors.Join(err, prepared.Close(), sourcePin.Close())
			}
			writer = newWriter
		}
		entry, err := writer.AppendPrepared(ctx, prepared)
		if err != nil {
			return errors.Join(err, sourcePin.Close())
		}
		sources = append(sources, packedSource{candidate: candidate, path: source, pin: sourcePin, entry: entry})
		rawBytes += int64(entry.RawLen)
		if writer.Full() || (opts.MaxBytes > 0 && rawBytes >= opts.MaxBytes) {
			if err := seal(); err != nil {
				return err
			}
			if opts.MaxBytes > 0 && rawBytes >= opts.MaxBytes {
				stats.BudgetExhausted = true
				return nil
			}
		}
	}
	return seal()
}

func (m *Maintainer) prepareCandidate(
	ctx context.Context, candidate Candidate, openPin identityPinOpener,
) (*pack.PreparedBlob, string, identityPin, bool, error) {
	id, err := pack.ParseBlobID(candidate.Hash.String())
	if err != nil {
		return nil, "", nil, false, err
	}
	if candidate.Size < 0 {
		return nil, "", nil, false, fmt.Errorf("%w: negative loose logical size %d", ErrContentMismatch, candidate.Size)
	}
	if candidate.Size > m.limits.BlobBytes {
		return nil, "", nil, false, newLimitError(
			LimitBlobRawBytes, uint64(candidate.Size), uint64(m.limits.BlobBytes),
		)
	}
	var corrupt error
	var selected *pack.PreparedBlob
	var selectedPath string
	var selectedPin identityPin
	seenPaths := make(map[string]struct{}, len(candidate.Paths))
	for pathIndex, candidatePath := range candidate.Paths {
		if m.beforeCandidatePath != nil {
			m.beforeCandidatePath(pathIndex)
		}
		if err := ctx.Err(); err != nil {
			if selected != nil {
				err = errors.Join(err, selected.Close(), closeIdentityPin(selectedPin))
			}
			return nil, "", nil, false, err
		}
		path := candidatePath
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.layout.Root(), filepath.FromSlash(path))
		}
		path = filepath.Clean(path)
		if _, duplicate := seenPaths[path]; duplicate {
			continue
		}
		seenPaths[path] = struct{}{}
		encoding := LooseEncodingRaw
		if canonicalLoosePathEqual(path, m.layout.CompressedLoosePath(candidate.Hash)) {
			encoding = LooseEncodingZstd
		}
		object, identity, err := openCandidateLooseObject(path, candidate.Hash, candidate.Size, encoding)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			corrupt = errors.Join(corrupt, err)
			continue
		}
		if object.storedSize > m.limits.BlobBytes {
			corrupt = errors.Join(
				corrupt,
				newLimitError(
					LimitBlobStoredBytes,
					uint64(object.storedSize),
					uint64(m.limits.BlobBytes),
				),
				object.file.Close(),
			)
			continue
		}
		var pin identityPin
		if selected == nil {
			pin, err = m.pinOpenCandidate(path, identity, openPin)
			if err != nil {
				corrupt = errors.Join(corrupt, err, object.file.Close())
				continue
			}
		}
		stream, err := newLooseVerifiedStream(ctx, candidate.Hash, object)
		if err != nil {
			corrupt = errors.Join(corrupt, err, closeIdentityPin(pin))
			continue
		}
		var prepared *pack.PreparedBlob
		var prepareErr error
		if selected == nil {
			prepared, prepareErr = pack.PrepareBlob(ctx, stream, uint64(candidate.Size), pack.DefaultZstdLevel, pack.AppendStreamOptions{
				ExpectedID: &id, ScratchDir: m.layout.PacksDir(),
			})
		}
		verifyErr := stream.Verify()
		closeErr := stream.Close()
		if err := errors.Join(prepareErr, verifyErr, closeErr); err != nil {
			if prepared != nil {
				err = errors.Join(err, prepared.Close())
			}
			err = errors.Join(err, closeIdentityPin(pin))
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if selected != nil {
					err = errors.Join(err, selected.Close(), closeIdentityPin(selectedPin))
				}
				return nil, "", nil, false, err
			}
			corrupt = errors.Join(corrupt, err)
			continue
		}
		if selected == nil && prepared == nil {
			return nil, "", nil, false, errors.Join(
				fmt.Errorf("packstore: preparing candidate %s returned no result", candidate.Hash),
				closeIdentityPin(pin),
			)
		}
		if selected == nil {
			selected = prepared
			selectedPath = path
			selectedPin = pin
		}
	}
	if selected != nil {
		return selected, selectedPath, selectedPin, true, nil
	}
	return nil, "", nil, false, corrupt
}

func (m *Maintainer) pinOpenCandidate(
	path string, expected fs.FileInfo, openPin identityPinOpener,
) (identityPin, error) {
	pin, pinned, err := openPin(path)
	if err != nil {
		return nil, err
	}
	if pin == nil || pinned == nil {
		return nil, errors.Join(fmt.Errorf("packstore: identity pin for %s has incomplete state", path), closeIdentityPin(pin))
	}
	if !os.SameFile(expected, pinned) {
		return nil, errors.Join(errIdentityChanged, pin.Close())
	}
	return pin, nil
}

func closeIdentityPin(pin identityPin) error {
	if pin == nil {
		return nil
	}
	return pin.Close()
}

func closePackedSourcePins(sources []packedSource) error {
	var resultErr error
	for _, source := range sources {
		resultErr = errors.Join(resultErr, closeIdentityPin(source.pin))
	}
	return resultErr
}

func openCandidateLooseObject(
	path string,
	hash Hash,
	expectedSize int64,
	encoding LooseEncoding,
) (*looseObject, fs.FileInfo, error) {
	return openLooseObjectAtPath(path, hash, &expectedSize, encoding)
}

func openLooseObjectAtPath(
	path string,
	hash Hash,
	expectedSize *int64,
	encoding LooseEncoding,
) (*looseObject, fs.FileInfo, error) {
	identity, err := snapshotPathIdentity(path)
	if err != nil {
		return nil, nil, err
	}
	if err := validateRegularNoFollow(path, identity); err != nil {
		return nil, nil, err
	}
	f, info, err := openLooseFile(path)
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(identity, info) {
		return nil, nil, errors.Join(errIdentityChanged, f.Close())
	}
	logicalSize := info.Size()
	if encoding == LooseEncodingZstd {
		header := make([]byte, compressedLooseHeaderSize)
		if _, err := io.ReadFull(f, header); err != nil {
			return nil, nil, errors.Join(
				fmt.Errorf("%w: read compressed loose header: %w", ErrContentMismatch, err),
				f.Close(),
			)
		}
		logicalSize, err = decodeCompressedLooseHeader(header)
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("%w: %w", ErrContentMismatch, err), f.Close())
		}
	}
	if expectedSize != nil && logicalSize != *expectedSize {
		return nil, nil, errors.Join(
			fmt.Errorf("%w: loose logical size is %d, want %d", ErrContentMismatch, logicalSize, *expectedSize),
			f.Close(),
		)
	}
	return &looseObject{
		file: f, encoding: encoding, logicalSize: logicalSize, storedSize: info.Size(),
	}, identity, nil
}

func verifyLoosePath(ctx context.Context, path string, hash Hash, limit int64) error {
	_, err := verifyLoosePathIdentity(ctx, path, hash, limit, LooseEncodingRaw)
	return err
}

func verifyLoosePathIdentity(
	ctx context.Context,
	path string,
	hash Hash,
	limit int64,
	encoding LooseEncoding,
) (fs.FileInfo, error) {
	pin, _, err := verifyLoosePathPinned(ctx, path, hash, limit, encoding, openLooseMaintenanceVerificationPin)
	if err != nil {
		return nil, err
	}
	identity, statErr := pin.Stat()
	return identity, errors.Join(statErr, pin.Close())
}

func verifyLoosePathPinned(
	ctx context.Context,
	path string,
	hash Hash,
	limit int64,
	encoding LooseEncoding,
	openPin identityPinOpener,
) (identityPin, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	object, identity, err := openLooseObjectAtPath(path, hash, nil, encoding)
	if err != nil {
		return nil, 0, err
	}
	size := object.logicalSize
	if size < 0 || size > limit {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobRawBytes, uint64(size), uint64(limit)),
			object.file.Close(),
		)
	}
	if object.storedSize > limit {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobStoredBytes, uint64(object.storedSize), uint64(limit)),
			object.file.Close(),
		)
	}
	pin, pinned, err := openPin(path)
	if err != nil {
		return nil, 0, errors.Join(err, object.file.Close())
	}
	if !os.SameFile(identity, pinned) {
		return nil, 0, errors.Join(errIdentityChanged, object.file.Close(), pin.Close())
	}
	stream, err := newLooseVerifiedStream(ctx, hash, object)
	if err != nil {
		return nil, 0, errors.Join(err, pin.Close())
	}
	if err := errors.Join(stream.Verify(), stream.Close()); err != nil {
		return nil, 0, errors.Join(err, pin.Close())
	}
	return pin, size, nil
}

func (m *Maintainer) findAnyValidCanonicalLoose(
	ctx context.Context, hash Hash,
) (int64, bool, error) {
	for _, candidate := range []struct {
		path     string
		encoding LooseEncoding
	}{
		{path: m.layout.CompressedLoosePath(hash), encoding: LooseEncodingZstd},
		{path: m.layout.LoosePath(hash), encoding: LooseEncodingRaw},
	} {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		pin, size, err := verifyLoosePathPinned(
			ctx,
			candidate.path,
			hash,
			m.limits.BlobBytes,
			candidate.encoding,
			openLooseMaintenanceVerificationPin,
		)
		if err == nil {
			if err := pin.Close(); err != nil {
				return 0, false, err
			}
			return size, true, nil
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, false, err
		}
	}
	return 0, false, nil
}

func (m *Maintainer) hasAuthoritativeCanonicalLoose(ctx context.Context, hash Hash) (bool, error) {
	for _, candidate := range []struct {
		path     string
		encoding LooseEncoding
	}{
		{path: m.layout.CompressedLoosePath(hash), encoding: LooseEncodingZstd},
		{path: m.layout.LoosePath(hash), encoding: LooseEncodingRaw},
	} {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		_, err := verifyLoosePathIdentity(ctx, candidate.path, hash, m.limits.BlobBytes, candidate.encoding)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
	}
	return false, nil
}

func verifyLooseFile(ctx context.Context, f *os.File, info fs.FileInfo, hash Hash) error {
	size := info.Size()
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	source := &contextio.Reader{Context: ctx, Reader: f}
	reader := io.LimitReader(source, size)
	written, err := io.CopyBuffer(digest, reader, buffer)
	if err != nil {
		return err
	}
	if written != size {
		return ErrContentMismatch
	}
	var probe [1]byte
	if n, err := source.Read(probe[:]); n != 0 || err == nil {
		return ErrContentMismatch
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		return errors.Join(err, errIdentityChanged)
	}
	if after.Size() != size {
		return ErrContentMismatch
	}
	if hex.EncodeToString(digest.Sum(nil)) != hash.String() {
		return ErrContentMismatch
	}
	return nil
}

func (m *Maintainer) sealAndCommit(
	ctx context.Context,
	writer *pack.Writer,
	sources []packedSource,
	stats *PackStats,
	policy packCandidatePolicy,
) (resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr, closePackedSourcePins(sources))
	}()
	packID := writer.ID()
	path := m.layout.PackPath(packID)
	entries, err := writer.Seal(path)
	if err != nil {
		return err
	}
	if err := validateSealedOutput(path, m.limits); err != nil {
		return err
	}
	if err := ValidateIndexEntries(indexEntriesFromPack(packID, entries)); err != nil {
		return err
	}
	record := PackRecord{PackID: packID, EntryCount: int64(len(entries)), CreatedAt: time.Now().UTC()}
	adoptions := make([]Adoption, len(sources))
	for i, source := range sources {
		entry := indexFromPack(packID, source.entry)
		record.StoredBytes += entry.StoredLen
		adoptions[i] = Adoption{Entry: entry, OriginalHashes: source.candidate.OriginalHashes}
	}
	if err := policy.commit(ctx, record, adoptions); err != nil {
		return err
	}
	stats.PacksSealed++
	stats.BlobsPacked += len(sources)
	if policy.replaceMappings {
		stats.MappingsPruned += int64(len(sources))
	}
	var cleanupErr error
	for index := range sources {
		source := sources[index]
		stats.BytesPacked += int64(source.entry.RawLen)
		if !policy.removeSource {
			continue
		}
		verifiedIdentity, statErr := source.pin.Stat()
		sources[index].pin = nil
		if statErr != nil {
			cleanupErr = errors.Join(cleanupErr, statErr, source.pin.Close())
			continue
		}
		removalPin, removalIdentity, openErr := m.openIdentityPin(source.path)
		closeErr := source.pin.Close()
		if closeErr != nil {
			cleanupErr = errors.Join(cleanupErr, closeErr, closeIdentityPin(removalPin))
			continue
		}
		if openErr != nil {
			if errors.Is(openErr, fs.ErrNotExist) || errors.Is(openErr, fs.ErrPermission) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf(
				"packstore: open packed loose source %s for removal: %w", source.path, openErr,
			))
			continue
		}
		if !os.SameFile(verifiedIdentity, removalIdentity) {
			cleanupErr = errors.Join(cleanupErr, errIdentityChanged, removalPin.Close())
			continue
		}
		// Removal owns and closes the removal pin on every return path.
		if _, err := removeLoosePathPinned(source.path, removalPin); err != nil &&
			!errors.Is(err, errLooseRemovalUnavailable) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf(
				"packstore: remove packed loose source %s: %w", source.path, err,
			))
		}
	}
	return cleanupErr
}

func checkPlainOutput(limits Limits, dataEnd, nextStored uint64, entryCount int) error {
	if dataEnd > ^uint64(0)-nextStored {
		return newLimitError(LimitPackContainerBytes, ^uint64(0), uint64(limits.PackBytes))
	}
	containerBytes, footerBytes, err := pack.PlainPackSize(dataEnd+nextStored, entryCount)
	if err != nil {
		return err
	}
	if entryCount > limits.PackEntries {
		return newLimitError(LimitPackEntryCount, uint64(entryCount), uint64(limits.PackEntries))
	}
	if footerBytes > uint64(limits.FooterBytes) {
		return newLimitError(LimitPackFooterBytes, footerBytes, uint64(limits.FooterBytes))
	}
	if containerBytes > uint64(limits.PackBytes) {
		return newLimitError(LimitPackContainerBytes, containerBytes, uint64(limits.PackBytes))
	}
	return nil
}

func validateSealedOutput(path string, limits Limits) error {
	reader, err := OpenMaintenancePack(path, limits)
	if err != nil {
		return err
	}
	return reader.Close()
}

func (m *Maintainer) sweepLoose(ctx context.Context, refs map[Hash]Reference, allowOrphanRemoval bool, stats *PackStats) error {
	indexed, err := m.catalog.ListIndexed(ctx)
	if err != nil {
		return err
	}
	for _, entry := range indexed {
		hasCandidate, err := m.hasCanonicalLooseCandidate(ctx, entry.Hash)
		if err != nil {
			return err
		}
		if !hasCandidate {
			continue
		}
		if _, _, err := m.store.ReadBounded(ctx, entry.Hash, m.limits.BlobBytes); err != nil {
			continue
		}
		present := 0
		verified := 0
		for _, candidate := range []struct {
			path     string
			encoding LooseEncoding
		}{
			{path: m.layout.LoosePath(entry.Hash), encoding: LooseEncodingRaw},
			{path: m.layout.CompressedLoosePath(entry.Hash), encoding: LooseEncodingZstd},
		} {
			physical, err := snapshotPathIdentity(candidate.path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err := validateRegularNoFollow(candidate.path, physical); err != nil {
				continue
			}
			present++
			verificationPin, _, err := verifyLoosePathPinned(
				ctx,
				candidate.path,
				entry.Hash,
				m.limits.BlobBytes,
				candidate.encoding,
				openLooseMaintenanceVerificationPin,
			)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				continue
			}
			verified++
			verifiedIdentity, statErr := verificationPin.Stat()
			removalPin, removalIdentity, openErr := m.openIdentityPin(candidate.path)
			if openErr != nil {
				if closeErr := verificationPin.Close(); closeErr != nil {
					return closeErr
				}
				continue
			}
			closeErr := verificationPin.Close()
			if err := errors.Join(statErr, closeErr); err != nil {
				return errors.Join(err, removalPin.Close())
			}
			if !os.SameFile(verifiedIdentity, removalIdentity) {
				if err := removalPin.Close(); err != nil {
					return err
				}
				continue
			}
			removed, removeErr := removeLoosePathPinned(candidate.path, removalPin)
			if errors.Is(removeErr, errLooseRemovalUnavailable) {
				continue
			}
			if removeErr != nil {
				return fmt.Errorf("packstore: sweep loose content: %w", removeErr)
			}
			if removed {
				stats.LooseSwept++
			}
		}
		if present > 0 && verified == 0 {
			stats.BlobsCorrupt++
		}
	}
	entries, err := os.ReadDir(m.layout.Root())
	if err != nil {
		return err
	}
	if !allowOrphanRemoval {
		stats.LooseOrphanSweepSuppressed = true
		return nil
	}
	for _, shard := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !shard.IsDir() || len(shard.Name()) != 2 || shard.Name() == "pa" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(m.layout.Root(), shard.Name()))
		if err != nil {
			return err
		}
		for _, file := range files {
			hash, encoding, ok := parseCanonicalLooseName(file.Name())
			if !ok || hash.String()[:2] != shard.Name() || !file.Type().IsRegular() {
				continue
			}
			if _, live := refs[hash]; live {
				continue
			}
			path := m.layout.LoosePath(hash)
			if encoding == LooseEncodingZstd {
				path = m.layout.CompressedLoosePath(hash)
			}
			if filepath.Clean(path) != filepath.Clean(filepath.Join(m.layout.Root(), shard.Name(), file.Name())) {
				continue
			}
			removed, err := removeLoosePath(path, openLooseIdentityPin)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if errors.Is(err, ErrContentMismatch) {
				continue
			}
			if err != nil {
				return err
			}
			if removed {
				stats.LooseOrphansRemoved++
			}
		}
	}
	return nil
}

// hasCanonicalLooseCandidate is an authorization-free, no-follow preflight.
// A negative result only defers work; deletion still requires a second open,
// full verification, and a live identity pin after packed authority verifies.
func (m *Maintainer) hasCanonicalLooseCandidate(ctx context.Context, hash Hash) (bool, error) {
	for _, path := range []string{m.layout.LoosePath(hash), m.layout.CompressedLoosePath(hash)} {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		identity, err := snapshotPathIdentity(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if err := validateRegularNoFollow(path, identity); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func parseCanonicalLooseName(name string) (Hash, LooseEncoding, bool) {
	encoding := LooseEncodingRaw
	hashName := name
	if strings.HasSuffix(name, ".zst") {
		encoding = LooseEncodingZstd
		hashName = strings.TrimSuffix(name, ".zst")
	}
	hash, err := ParseHash(hashName)
	return hash, encoding, err == nil
}

// removeLoosePathPinned moves the current directory entry into an exclusive
// sibling aside before deciding whether to delete it. The move closes the
// validation-to-unlink race at path: a replacement is restored without
// clobbering any still-newer occupant, while only the expected identity is
// discarded.
func removeLoosePath(path string, openPin identityPinOpener) (removed bool, resultErr error) {
	pin, _, err := openPin(path)
	if err != nil {
		return false, err
	}
	return removeLoosePathPinned(path, pin)
}

func removeLoosePathPinned(path string, pin identityPin) (bool, error) {
	return removeLoosePathPinnedWithOwnership(path, pin, true)
}

func removeLoosePathPinnedWithOwnership(
	path string,
	pin identityPin,
	consumePin bool,
) (removed bool, resultErr error) {
	pinOpen := true
	defer func() {
		if consumePin && pinOpen {
			resultErr = errors.Join(resultErr, pin.Close())
		}
	}()
	identity, err := pin.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect pinned loose content %s: %w", path, err)
	}
	removalUnavailable := func(cause error) (bool, error) {
		current, currentErr := snapshotPathIdentity(path)
		if currentErr != nil {
			return false, errors.Join(cause, currentErr)
		}
		if validationErr := validateRegularNoFollow(path, current); validationErr != nil {
			return false, errors.Join(cause, validationErr)
		}
		if !os.SameFile(identity, current) {
			return false, errors.Join(cause, errIdentityChanged)
		}
		if consumePin {
			closeErr := pin.Close()
			pinOpen = false
			if closeErr != nil {
				return false, errors.Join(cause, closeErr)
			}
		}
		return false, errors.Join(errLooseRemovalUnavailable, cause)
	}
	asideDir, err := makeLooseRemovalAside(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return removalUnavailable(err)
		}
		return false, err
	}
	aside := filepath.Join(asideDir, "claimed")
	beforeLooseRemovalClaim(path)
	if err := claimLooseRemovalPath(path, aside); err != nil {
		cleanupErr := removeLooseRemovalAside(asideDir)
		if errors.Is(err, fs.ErrNotExist) {
			return false, cleanupErr
		}
		if errors.Is(err, fs.ErrPermission) && cleanupErr == nil {
			return removalUnavailable(fmt.Errorf("claim %s for removal: %w", path, err))
		}
		return false, errors.Join(
			fmt.Errorf("claim %s for removal: %w", path, err),
			cleanupErr,
		)
	}
	claimed, err := snapshotPathIdentity(aside)
	if err != nil {
		return false, fmt.Errorf(
			"inspect claimed loose content %s (preserved at %s): %w", path, aside, err,
		)
	}
	if !os.SameFile(identity, claimed) {
		restoreErr := restoreLooseRemovalClaim(path, aside, claimed)
		if restoreErr != nil {
			return false, errors.Join(errIdentityChanged, restoreErr)
		}
		return false, errIdentityChanged
	}
	if err := validateRegularNoFollow(aside, claimed); err != nil {
		return false, fmt.Errorf(
			"validate claimed loose content %s (preserved at %s): %w", path, aside, err,
		)
	}
	if err := removePinnedLooseClaim(pin, aside); err != nil {
		removeErr := fmt.Errorf("remove claimed loose content %s: %w", aside, err)
		restoreErr := restoreLooseRemovalClaim(path, aside, claimed)
		if errors.Is(err, fs.ErrPermission) && restoreErr == nil {
			return removalUnavailable(removeErr)
		}
		return false, errors.Join(removeErr, restoreErr)
	}
	if consumePin {
		closeErr := pin.Close()
		pinOpen = false
		if closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close removed loose content pin %s: %w", aside, closeErr))
		}
	}
	if err := removeLooseRemovalAside(asideDir); err != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("remove empty loose removal aside %s: %w", asideDir, err))
	}
	return true, resultErr
}

// makeLooseRemovalAside uses an exclusive directory rather than renaming
// directly to a random sibling file. atomicfile.Replace replaces existing
// destinations; the private directory makes the final claim name known-absent.
func makeLooseRemovalAside(path string) (string, error) {
	const attempts = 8
	for range attempts {
		aside := filepath.Join(
			filepath.Dir(path),
			"."+filepath.Base(path)+".remove-"+pack.NewPackID(),
		)
		if err := createLooseRemovalAside(aside); err == nil {
			return aside, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("create loose removal aside for %s: %w", path, err)
		}
	}
	return "", fmt.Errorf("create unique loose removal aside for %s: %w", path, fs.ErrExist)
}

// restoreLooseRemovalClaim puts a foreign claimed entry back without replacing
// a newer entry at path. Failures preserve the claim at the path named in the
// returned error for diagnosis and manual recovery.
func restoreLooseRemovalClaim(path, aside string, claimed fs.FileInfo) error {
	current, err := snapshotPathIdentity(aside)
	if err != nil {
		return fmt.Errorf("inspect foreign loose removal claim %s: %w", aside, err)
	}
	if !os.SameFile(claimed, current) {
		return fmt.Errorf("%w: foreign loose removal claim %s changed before restore", errIdentityChanged, aside)
	}
	switch {
	case claimed.Mode().IsRegular():
		if err := linkLooseRemovalClaim(path, aside, claimed); err != nil {
			return err
		}
	case claimed.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(aside)
		if err != nil {
			return fmt.Errorf("read foreign loose symlink %s for restore: %w", aside, err)
		}
		if err := os.Symlink(target, path); err != nil {
			return fmt.Errorf("restore foreign loose symlink from %s to %s without clobbering: %w", aside, path, err)
		}
		// Symlink creates with no replacement are atomic. Once this succeeds,
		// any subsequent removal or replacement at path is a later operation.
		afterLooseRemovalRestorePublish(path)
	default:
		return fmt.Errorf("foreign loose content at %s has unsupported mode %s; preserved at %s", path, claimed.Mode(), aside)
	}
	current, err = snapshotPathIdentity(aside)
	if err != nil {
		return fmt.Errorf("recheck restored foreign loose claim %s: %w", aside, err)
	}
	if !os.SameFile(claimed, current) {
		return fmt.Errorf("%w: restored foreign loose claim %s changed before cleanup", errIdentityChanged, aside)
	}
	if err := removeLooseCanonicalFile(aside); err != nil {
		return fmt.Errorf("clean restored foreign loose claim %s: %w", aside, err)
	}
	if err := removeLooseRemovalAside(filepath.Dir(aside)); err != nil {
		return fmt.Errorf("remove restored loose removal aside %s: %w", filepath.Dir(aside), err)
	}
	return nil
}

// linkLooseRemovalClaim restores the exact claimed inode with a no-clobber hard
// link. The namespace identity pin does not require content-read access.
// Writers that already hold the inode therefore remain attached to the restored
// name, and no copied snapshot can overwrite their later changes.
func linkLooseRemovalClaim(path, aside string, claimed fs.FileInfo) (resultErr error) {
	source, sourceIdentity, err := openLooseRestorationIdentityPin(aside)
	if err != nil {
		return fmt.Errorf("pin foreign loose content %s for restore: %w", aside, err)
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	if !os.SameFile(claimed, sourceIdentity) {
		return errors.Join(errIdentityChanged, fmt.Errorf("foreign loose removal claim %s changed while opening", aside))
	}
	beforeLooseRemovalRestorePublish(aside, path)
	if err := publishLooseRemovalRestoreFile(aside, path); err != nil {
		return fmt.Errorf("publish restored foreign loose content from %s to %s without clobbering: %w", aside, path, err)
	}
	pinned, pinErr := source.Stat()
	canonical, canonicalErr := snapshotPathIdentity(path)
	current, currentErr := snapshotPathIdentity(aside)
	if err := errors.Join(pinErr, canonicalErr, currentErr); err != nil {
		return fmt.Errorf("recheck linked foreign loose claim %s: %w", aside, err)
	}
	if !os.SameFile(claimed, pinned) ||
		!os.SameFile(pinned, canonical) ||
		!os.SameFile(pinned, current) {
		return fmt.Errorf("%w: linked foreign loose claim %s changed identity", errIdentityChanged, aside)
	}
	// The exact claimed inode is now atomically visible at path. Later writes,
	// removal, or replacement are later actions and cannot make claim cleanup
	// discard a stale copy.
	afterLooseRemovalRestorePublish(path)
	return nil
}

// canonicalLoosePathEqual is lexical only: Windows folds case, while no
// platform resolves symlink aliases or broadens the canonical namespace.
var canonicalLoosePathEqual = func(left, right string) bool {
	return canonicalLoosePathEqualForOS(runtime.GOOS, left, right)
}

func canonicalLoosePathEqualForOS(goos, left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if goos == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func indexFromPack(packID string, entry pack.Entry) IndexEntry {
	hash, _ := ParseHash(entry.ID.String())
	return IndexEntry{
		Hash: hash, PackID: packID, Offset: int64(entry.Offset),
		StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen),
		Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
	}
}

func indexEntriesFromPack(packID string, entries []pack.Entry) []IndexEntry {
	result := make([]IndexEntry, len(entries))
	for i, entry := range entries {
		result[i] = indexFromPack(packID, entry)
	}
	return result
}
