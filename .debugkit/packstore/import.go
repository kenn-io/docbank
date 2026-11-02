package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
)

// ImportSelection identifies one application-live entry selected from a source
// pack. Its metadata must exactly match the source pack's authoritative footer.
type ImportSelection struct {
	Hash      Hash
	RawLen    int64
	Offset    uint64
	StoredLen uint64
	Flags     uint8
}

// ImportPack describes one immutable source pack and its application-live
// selections. SourcePath is read-only; importing copies and never moves source
// bytes. PackID is retained for a stable, idempotent destination on retry.
type ImportPack struct {
	PackID     string
	SourcePath string
	Selections []ImportSelection
}

// FallbackReason identifies why direct pack import was declined. It is not an
// integrity verdict: every declined selection must still be materialized by an
// encoding-aware loose path that authenticates and content-hash verifies it.
type FallbackReason string

const (
	// FallbackPackContainerLimit declines every selection in an oversized pack.
	FallbackPackContainerLimit FallbackReason = "pack_container_limit"
	// FallbackPackFooterLimit declines every selection when its footer is too large.
	FallbackPackFooterLimit FallbackReason = "pack_footer_limit"
	// FallbackPackEntryCountLimit declines every selection when it has too many entries.
	FallbackPackEntryCountLimit FallbackReason = "pack_entry_count_limit"
	// FallbackPackEncoding declines direct import of a recognizable pack whose
	// version or encoding settings this plain importer cannot verify. The caller
	// must authenticate and hash every selected blob through its loose reader.
	FallbackPackEncoding FallbackReason = "pack_encoding"
	// FallbackPackPublication declines every selection when the target
	// filesystem cannot atomically publish an immutable pack without replacing
	// a same-ID destination.
	FallbackPackPublication FallbackReason = "pack_publication"
	// FallbackBlobLimit declines one selected entry that exceeds the blob ceiling.
	FallbackBlobLimit FallbackReason = "blob_limit"
)

// ImportFallback records content that must be restored and verified loose.
// Hash is empty when the reason applies to every selection in the pack.
type ImportFallback struct {
	PackID string
	Hash   Hash
	Reason FallbackReason
}

// ImportOptions configures compatibility checks and catalog metadata for the
// target store.
type ImportOptions struct {
	// Limits are the target store's configured maintenance ceilings.
	Limits Limits
	// CreatedAt is written to imported PackRecords. Restore callers normally
	// use restore time so age-based maintenance does not immediately repack
	// newly imported containers.
	CreatedAt time.Time
}

// ImportStats summarizes the compatible subset selected for import and the
// content that a caller must restore loose.
type ImportStats struct {
	// PackedPacks is the number of whole immutable packs durably published or
	// reused and included in the prepared catalog replacement.
	PackedPacks int
	// PackedBlobs is the number of selected hashes that will receive packed
	// authority. It excludes unselected footer entries and loose fallbacks.
	PackedBlobs int
	// Fallbacks records pack-wide and per-hash compatibility declines.
	Fallbacks []ImportFallback
}

// importRootLink is a test seam for filesystems that do not support hard
// links. Production code must not replace it.
var importRootLink = func(root *os.Root, oldName, newName string) error {
	return root.Link(oldName, newName)
}

var (
	importLinkUnsupported    = isImportLinkUnsupported
	importAfterSourceOpen    = func(string) error { return nil }
	importVerifySelectedBlob = func(ctx context.Context, reader *MaintenancePackReader, hash Hash) error {
		stream, err := reader.OpenBlob(ctx, hash)
		if err != nil {
			return err
		}
		return errors.Join(stream.Verify(), stream.Close())
	}
	errImportPublicationUnsupported = errors.New("packstore: atomic pack publication unsupported")
)

// RestoreCatalog atomically replaces the packed-storage authority of a
// restored application catalog. Implementations must make repeated calls with
// the same records and adoptions idempotent so a restore can retry after a
// crash without relying on uncataloged pack files surviving maintenance.
type RestoreCatalog interface {
	// ReplaceRestoredPacks atomically replaces all packed records and selected
	// mappings for the restored catalog. Records contain whole-footer totals;
	// adoptions contain only application-live hashes authorized for reads.
	ReplaceRestoredPacks(context.Context, []PackRecord, []Adoption) error
}

type preparedImportPack struct {
	pack       ImportPack
	entries    []pack.Entry
	selections []ImportSelection
	fallbacks  []ImportFallback
	record     PackRecord
	adoptions  []Adoption
}

// PreparedImport is a verified, authority-free import plan whose packs have
// already been durably published or safely reused. A prepared hash is not
// readable through a Store until a later catalog commit grants authority.
type PreparedImport struct {
	packedHashes []Hash
	stats        ImportStats
	packs        []preparedImportPack
	createdAt    time.Time
}

// PackedHashes returns a copy of the selected hashes verified as compatible.
func (p *PreparedImport) PackedHashes() []Hash {
	if p == nil {
		return nil
	}
	return append([]Hash(nil), p.packedHashes...)
}

// Stats returns a copy of the preparation summary, including every fallback
// the caller must materialize and verify loose before Commit.
func (p *PreparedImport) Stats() ImportStats {
	if p == nil {
		return ImportStats{}
	}
	stats := p.stats
	stats.Fallbacks = append([]ImportFallback(nil), p.stats.Fallbacks...)
	return stats
}

// Commit grants catalog authority to all durably published packs with one
// application transaction. Callers must first durably materialize all Stats
// fallbacks through a real authenticated, content-hash-verifying loose path.
// Commit may be called again after success or failure; RestoreCatalog's
// replacement semantics make identical retries idempotent.
func (p *PreparedImport) Commit(ctx context.Context, catalog RestoreCatalog) error {
	if p == nil {
		return errors.New("packstore: prepared import is nil")
	}
	if catalog == nil {
		return errors.New("packstore: restore catalog is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	records := make([]PackRecord, 0, len(p.packs))
	var adoptions []Adoption
	for _, imported := range p.packs {
		records = append(records, imported.record)
		for _, adoption := range imported.adoptions {
			owned := adoption
			owned.OriginalHashes = append([]string(nil), adoption.OriginalHashes...)
			adoptions = append(adoptions, owned)
		}
	}
	if err := catalog.ReplaceRestoredPacks(ctx, records, adoptions); err != nil {
		return fmt.Errorf("packstore: commit restored pack catalog: %w", err)
	}
	return nil
}

// PrepareImport validates plain version-1 source pack compatibility and
// verifies every selected entry it can consume within the target's configured
// bounds. Compatible packs are streamed once into the production sharded
// layout, synced, published without replacement, reopened, and verified. A
// fallback only declines direct import; the caller must materialize every
// declined selection through an encoding-aware, authenticated, content-hashed
// loose path before treating restore as successful. PrepareImport does not
// grant catalog authority. Durable publication and catalog commit are separate
// operations so applications can preserve publish-before-authority ordering.
// Existing same-ID files are reused only when byte-identical; collisions fail
// closed. On filesystems without atomic hard-link publication, new packs fall
// back rather than using a racy replacement rename.
func PrepareImport(
	ctx context.Context,
	target *os.Root,
	contentDir string,
	packs []ImportPack,
	opts ImportOptions,
) (*PreparedImport, error) {
	if err := validateImportInputs(target, contentDir, packs, opts); err != nil {
		return nil, err
	}
	prepared := &PreparedImport{createdAt: opts.CreatedAt}
	seenPacks := make(map[string]struct{}, len(packs))
	seenHashes := make(map[Hash]struct{})
	for _, candidate := range packs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, duplicate := seenPacks[candidate.PackID]; duplicate {
			return nil, fmt.Errorf("packstore: duplicate import pack id %q", candidate.PackID)
		}
		seenPacks[candidate.PackID] = struct{}{}

		reader, err := OpenMaintenancePack(candidate.SourcePath, opts.Limits)
		if err != nil {
			if reason, compatible := importFallbackReason(err); compatible {
				if reason != FallbackPackEncoding {
					if verifyErr := verifyLimitedImportPack(ctx, target, candidate, opts.Limits); verifyErr != nil {
						return nil, fmt.Errorf("verify limited import pack %s: %w", candidate.PackID, verifyErr)
					}
				}
				prepared.stats.Fallbacks = append(prepared.stats.Fallbacks, ImportFallback{
					PackID: candidate.PackID,
					Reason: reason,
				})
				continue
			}
			return nil, fmt.Errorf("prepare import pack %s: %w", candidate.PackID, err)
		}

		packPlan, err := prepareImportPack(ctx, reader, candidate, opts.Limits, seenHashes)
		closeErr := reader.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close import pack %s: %w", candidate.PackID, closeErr)
		}
		if len(packPlan.selections) == 0 {
			prepared.stats.Fallbacks = append(prepared.stats.Fallbacks, packPlan.fallbacks...)
			continue
		}
		entries, err := publishAndVerifyImportPack(ctx, target, contentDir, packPlan, opts.Limits)
		if errors.Is(err, errImportPublicationUnsupported) {
			prepared.stats.Fallbacks = append(prepared.stats.Fallbacks, ImportFallback{
				PackID: candidate.PackID, Reason: FallbackPackPublication,
			})
			continue
		}
		if err != nil {
			return nil, err
		}
		packPlan.entries = entries
		packPlan.record, packPlan.adoptions, err = importCatalogPlan(packPlan, opts.CreatedAt)
		if err != nil {
			return nil, err
		}
		prepared.packs = append(prepared.packs, packPlan)
		prepared.stats.PackedPacks++
		prepared.stats.PackedBlobs += len(packPlan.selections)
		prepared.stats.Fallbacks = append(prepared.stats.Fallbacks, packPlan.fallbacks...)
		for _, selection := range packPlan.selections {
			prepared.packedHashes = append(prepared.packedHashes, selection.Hash)
		}
	}
	return prepared, nil
}

func publishAndVerifyImportPack(
	ctx context.Context,
	target *os.Root,
	contentDir string,
	plan preparedImportPack,
	limits Limits,
) ([]pack.Entry, error) {
	final := importPackPath(contentDir, plan.pack.PackID)
	parent := path.Dir(final)
	if err := mkdirAllImportSynced(target, parent); err != nil {
		return nil, fmt.Errorf("packstore: create import pack directory: %w", err)
	}
	staging := path.Join(contentDir, "packs", "."+plan.pack.PackID+"."+pack.NewPackID()+".staging")
	copyResult, err := copyImportSource(ctx, target, staging, plan.pack.SourcePath, limits.PackBytes)
	if err != nil {
		return nil, errors.Join(err, cleanupImportStaging(target, staging))
	}

	stagedReader, err := openRootMaintenancePack(target, staging, limits)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("packstore: verify copied import staging: %w", err), cleanupImportStaging(target, staging))
	}
	stagedEntries := stagedReader.Entries()
	if err := verifyImportSelections(ctx, stagedReader, plan, true); err != nil {
		return nil, errors.Join(fmt.Errorf("packstore: verify copied import staging: %w", err), stagedReader.Close(), cleanupImportStaging(target, staging))
	}
	stagedIdentity, statErr := stagedReader.reader.file.Stat()
	closeErr := stagedReader.Close()
	if statErr != nil {
		return nil, errors.Join(fmt.Errorf("packstore: stat verified import staging: %w", statErr), closeErr, cleanupImportStaging(target, staging))
	}
	if closeErr != nil {
		return nil, errors.Join(fmt.Errorf("packstore: close verified import staging: %w", closeErr), cleanupImportStaging(target, staging))
	}

	existing := false
	linkErr := importRootLink(target, staging, final)
	if linkErr == nil {
		if err := cleanupImportStaging(target, staging); err != nil {
			return nil, fmt.Errorf("packstore: pack published but staging cleanup failed: %w", err)
		}
		if err := syncImportRootDir(target, parent); err != nil {
			return nil, fmt.Errorf("packstore: pack published but directory sync failed: %w", err)
		}
	} else if errors.Is(linkErr, fs.ErrExist) {
		existing = true
		if err := cleanupImportStaging(target, staging); err != nil {
			return nil, fmt.Errorf("packstore: clean import staging after collision: %w", err)
		}
	} else if importLinkUnsupported(linkErr) {
		_, statErr := target.Lstat(final)
		if statErr == nil {
			existing = true
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return nil, errors.Join(
				fmt.Errorf("packstore: inspect final import path after unsupported link: %w", statErr),
				cleanupImportStaging(target, staging))
		}
		if err := cleanupImportStaging(target, staging); err != nil {
			return nil, fmt.Errorf("packstore: clean import staging after unsupported link: %w", err)
		}
		if !existing {
			return nil, fmt.Errorf("%w: pack %s: %w", errImportPublicationUnsupported, plan.pack.PackID, linkErr)
		}
	} else {
		return nil, errors.Join(
			fmt.Errorf("packstore: publish imported pack %s without replacement: %w", plan.pack.PackID, linkErr),
			cleanupImportStaging(target, staging))
	}

	reader, err := openRootMaintenancePack(target, final, limits)
	if err != nil {
		if existing {
			return nil, fmt.Errorf("packstore: publish collision for pack %s: final verification: %w", plan.pack.PackID, err)
		}
		return nil, fmt.Errorf("packstore: final verification for published pack %s: %w", plan.pack.PackID, err)
	}
	if existing {
		if err := verifyOpenImportFinalBytes(ctx, target, final, reader.reader.file, copyResult.digest, copyResult.size); err != nil {
			_ = reader.Close()
			return nil, fmt.Errorf("packstore: publish collision for pack %s: %w", plan.pack.PackID, err)
		}
	} else {
		finalIdentity, err := reader.reader.file.Stat()
		if err != nil || !os.SameFile(stagedIdentity, finalIdentity) {
			_ = reader.Close()
			return nil, errors.Join(errors.New("packstore: published pack identity changed before verification"), err)
		}
	}
	entries := reader.Entries()
	if err := verifyImportSelections(ctx, reader, plan, false); err != nil {
		_ = reader.Close()
		if existing {
			return nil, fmt.Errorf("packstore: publish collision for pack %s: final verification: %w", plan.pack.PackID, err)
		}
		return nil, fmt.Errorf("packstore: final verification for published pack %s: %w", plan.pack.PackID, err)
	}
	if existing {
		if err := syncImportRootDir(target, parent); err != nil {
			return nil, errors.Join(
				fmt.Errorf("packstore: sync reused pack directory before catalog authority: %w", err),
				reader.Close())
		}
	}
	if err := reader.Close(); err != nil {
		return nil, fmt.Errorf("packstore: close final imported pack %s: %w", plan.pack.PackID, err)
	}
	if len(entries) != len(stagedEntries) {
		return nil, fmt.Errorf("packstore: final imported pack %s footer changed after staging verification", plan.pack.PackID)
	}
	return entries, nil
}

type importCopyResult struct {
	digest [sha256.Size]byte
	size   int64
}

func copyImportSource(
	ctx context.Context,
	target *os.Root,
	staging string,
	sourcePath string,
	maxBytes int64,
) (result importCopyResult, resultErr error) {
	before, err := snapshotBoundedPackPathIdentity(sourcePath)
	if err != nil {
		return result, fmt.Errorf("packstore: inspect import source: %w", err)
	}
	if err := validateRegularNoFollow(sourcePath, before); err != nil {
		return result, fmt.Errorf("packstore: validate import source: %w", err)
	}
	source, err := openNoFollow(sourcePath, false)
	if err != nil {
		return result, fmt.Errorf("packstore: reopen import source: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return result, errors.Join(errors.New("packstore: import source mutation before copy"), err)
	}
	if opened.Size() > maxBytes {
		return result, fmt.Errorf("packstore: import source mutation exceeds configured pack limit: %d > %d", opened.Size(), maxBytes)
	}
	if err := importAfterSourceOpen(sourcePath); err != nil {
		return result, fmt.Errorf("packstore: prepare import source copy: %w", err)
	}
	staged, err := target.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return result, fmt.Errorf("packstore: create private import staging: %w", err)
	}
	stagedOpen := true
	defer func() {
		if stagedOpen {
			resultErr = errors.Join(resultErr, staged.Close())
		}
	}()
	hasher := sha256.New()
	boundedWriter := &importBoundedWriter{writer: io.MultiWriter(staged, hasher), remaining: maxBytes}
	written, err := io.CopyBuffer(boundedWriter, &contextio.Reader{Context: ctx, Reader: source}, make([]byte, 64<<10))
	if err != nil {
		if errors.Is(err, errImportSourceExceedsLimit) {
			return result, fmt.Errorf("packstore: import source mutation exceeds configured pack limit: %w", err)
		}
		return result, fmt.Errorf("packstore: stream import source: %w", err)
	}
	afterDescriptor, err := source.Stat()
	if err != nil || !sameImportSourceState(before, opened, afterDescriptor) {
		return result, errors.Join(errors.New("packstore: import source mutation during copy"), err)
	}
	afterPath, err := snapshotBoundedPackPathIdentity(sourcePath)
	if err != nil || !sameImportSourceState(before, opened, afterPath) {
		return result, errors.Join(errors.New("packstore: import source mutation after copy"), err)
	}
	if written != opened.Size() {
		return result, fmt.Errorf("packstore: import source mutation changed size from %d to %d", opened.Size(), written)
	}
	if err := staged.Sync(); err != nil {
		return result, fmt.Errorf("packstore: sync import staging: %w", err)
	}
	if err := staged.Close(); err != nil {
		return result, fmt.Errorf("packstore: close import staging: %w", err)
	}
	stagedOpen = false
	copy(result.digest[:], hasher.Sum(nil))
	result.size = written
	return result, nil
}

var errImportSourceExceedsLimit = errors.New("packstore: import source exceeds configured pack limit")

type importBoundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *importBoundedWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, errImportSourceExceedsLimit
	}
	allowed := min(int64(len(p)), w.remaining)
	n, err := w.writer.Write(p[:allowed])
	w.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, errImportSourceExceedsLimit
	}
	return n, nil
}

func cleanupImportStaging(target *os.Root, staging string) error {
	err := target.Remove(staging)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove import staging: %w", err)
	}
	if err := syncImportRootDir(target, path.Dir(staging)); err != nil {
		return fmt.Errorf("sync import staging parent after removal: %w", err)
	}
	return nil
}

func sameImportSourceState(before, opened, after fs.FileInfo) bool {
	return before != nil && opened != nil && after != nil &&
		os.SameFile(before, opened) && os.SameFile(opened, after) &&
		before.Size() == opened.Size() && opened.Size() == after.Size() &&
		before.ModTime().Equal(opened.ModTime()) && opened.ModTime().Equal(after.ModTime())
}

func verifyOpenImportFinalBytes(
	ctx context.Context,
	target *os.Root,
	name string,
	f *os.File,
	wantDigest [sha256.Size]byte,
	wantSize int64,
) error {
	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat final pack for verification: %w", err)
	}
	hasher := sha256.New()
	section := io.NewSectionReader(f, 0, opened.Size())
	size, err := io.CopyBuffer(hasher, &contextio.Reader{Context: ctx, Reader: section}, make([]byte, 64<<10))
	if err != nil {
		return fmt.Errorf("hash final pack: %w", err)
	}
	after, err := target.Lstat(name)
	if err != nil || !os.SameFile(opened, after) {
		return errors.Join(errors.New("final pack changed identity during verification"), err)
	}
	if size != wantSize || !bytes.Equal(hasher.Sum(nil), wantDigest[:]) {
		return errors.New("final bytes differ from import source")
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync final pack: %w", err)
	}
	return nil
}

func openRootMaintenancePack(target *os.Root, name string, limits Limits) (*MaintenancePackReader, error) {
	before, err := target.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("packstore: final pack is not an independent regular file")
	}
	f, err := target.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.Join(errors.New("packstore: final pack changed identity before preflight"), err, f.Close())
	}
	reader, err := openBoundedPackFile(f, limits)
	if err != nil {
		return nil, err
	}
	return &MaintenancePackReader{reader: reader, limits: limits}, nil
}

func verifyImportSelections(ctx context.Context, reader *MaintenancePackReader, plan preparedImportPack, verifyPayload bool) error {
	authoritative, err := indexImportSelections(reader.Entries(), plan.pack)
	if err != nil {
		return err
	}
	for _, selection := range plan.selections {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := authoritative[selection.Hash]; !ok {
			return fmt.Errorf("%w: selected blob %s is absent", pack.ErrCorrupt, selection.Hash)
		}
		if verifyPayload {
			if err := importVerifySelectedBlob(ctx, reader, selection.Hash); err != nil {
				return fmt.Errorf("verify selected blob %s: %w", selection.Hash, err)
			}
		}
	}
	return nil
}

func importCatalogPlan(plan preparedImportPack, createdAt time.Time) (PackRecord, []Adoption, error) {
	record := PackRecord{PackID: plan.pack.PackID, EntryCount: int64(len(plan.entries)), CreatedAt: createdAt}
	storedBytes, err := importFooterStoredBytes(plan.entries)
	if err != nil {
		return PackRecord{}, nil, err
	}
	record.StoredBytes = storedBytes
	entries := make(map[Hash]pack.Entry, len(plan.entries))
	for _, entry := range plan.entries {
		hash, err := ParseHash(entry.ID.String())
		if err != nil {
			return PackRecord{}, nil, err
		}
		entries[hash] = entry
	}
	adoptions := make([]Adoption, 0, len(plan.selections))
	for _, selection := range plan.selections {
		entry := entries[selection.Hash]
		adoptions = append(adoptions, Adoption{
			Entry: IndexEntry{
				Hash: selection.Hash, PackID: plan.pack.PackID,
				Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen),
				Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
			},
			OriginalHashes: []string{selection.Hash.String()},
		})
	}
	if err := record.Validate(); err != nil {
		return PackRecord{}, nil, err
	}
	for _, adoption := range adoptions {
		if err := adoption.Entry.Validate(); err != nil {
			return PackRecord{}, nil, err
		}
	}
	return record, adoptions, nil
}

func importFooterStoredBytes(entries []pack.Entry) (int64, error) {
	ordered := make([]pack.Entry, 0, len(entries))
	var storedBytes int64
	for _, entry := range entries {
		if entry.Offset > math.MaxInt64 || entry.StoredLen > math.MaxInt64 || entry.RawLen > math.MaxInt64 {
			return 0, fmt.Errorf("%w: imported footer entry exceeds catalog integer range", pack.ErrCorrupt)
		}
		end := entry.Offset + entry.StoredLen
		if end < entry.Offset {
			return 0, fmt.Errorf("%w: imported footer entry span overflows", pack.ErrCorrupt)
		}
		if entry.StoredLen > 0 {
			ordered = append(ordered, entry)
		}
		stored := int64(entry.StoredLen)
		if storedBytes > math.MaxInt64-stored {
			return 0, fmt.Errorf("%w: imported footer stored-byte total overflows", pack.ErrCorrupt)
		}
		storedBytes += stored
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Offset < ordered[j].Offset })
	var previousEnd uint64
	for i, entry := range ordered {
		if i > 0 && entry.Offset < previousEnd {
			return 0, fmt.Errorf("%w: imported footer entries overlap", pack.ErrCorrupt)
		}
		previousEnd = entry.Offset + entry.StoredLen
	}
	return storedBytes, nil
}

func importPackPath(contentDir, packID string) string {
	return path.Join(contentDir, "packs", packID[:2], packID+PackExt)
}

func validateImportInputs(target *os.Root, contentDir string, packs []ImportPack, opts ImportOptions) error {
	if target == nil {
		return errors.New("packstore: import target is nil")
	}
	if contentDir == "" || contentDir == ".." || path.IsAbs(contentDir) ||
		path.Clean(contentDir) != contentDir || strings.Contains(contentDir, `\`) ||
		strings.HasPrefix(contentDir, "../") {
		return fmt.Errorf("packstore: unsafe import content directory %q", contentDir)
	}
	if err := validateLimits(opts.Limits); err != nil {
		return err
	}
	if opts.CreatedAt.IsZero() {
		return errors.New("packstore: import creation time is zero")
	}
	for _, candidate := range packs {
		if !pack.IsValidPackID(candidate.PackID) {
			return fmt.Errorf("packstore: invalid import pack id %q", candidate.PackID)
		}
		if candidate.SourcePath == "" {
			return fmt.Errorf("packstore: import pack %s has empty source path", candidate.PackID)
		}
		if len(candidate.Selections) == 0 {
			return fmt.Errorf("packstore: import pack %s has no selections", candidate.PackID)
		}
		for _, selection := range candidate.Selections {
			if err := selection.Hash.Validate(); err != nil {
				return err
			}
			if selection.RawLen < 0 {
				return fmt.Errorf("packstore: negative selected raw length for %s", selection.Hash)
			}
		}
	}
	return nil
}

func importFallbackReason(err error) (FallbackReason, bool) {
	if errors.Is(err, pack.ErrUnsupportedVersion) || errors.Is(err, errUnsupportedMaintenanceEncoding) {
		return FallbackPackEncoding, true
	}
	limitErr, ok := errors.AsType[*LimitError](err)
	if !ok {
		return "", false
	}
	switch limitErr.Dimension {
	case LimitPackContainerBytes:
		return FallbackPackContainerLimit, true
	case LimitPackFooterBytes:
		return FallbackPackFooterLimit, true
	case LimitPackEntryCount:
		return FallbackPackEntryCountLimit, true
	default:
		return "", false
	}
}

func prepareImportPack(
	ctx context.Context,
	reader *MaintenancePackReader,
	candidate ImportPack,
	limits Limits,
	seenHashes map[Hash]struct{},
) (preparedImportPack, error) {
	entries := reader.Entries()
	if _, err := importFooterStoredBytes(entries); err != nil {
		return preparedImportPack{}, fmt.Errorf("prepare import pack %s footer: %w", candidate.PackID, err)
	}
	authoritative, err := indexImportSelections(entries, candidate)
	if err != nil {
		return preparedImportPack{}, err
	}
	ownedCandidate := candidate
	ownedCandidate.Selections = append([]ImportSelection(nil), candidate.Selections...)
	plan := preparedImportPack{pack: ownedCandidate, entries: entries}
	for _, selection := range candidate.Selections {
		if err := ctx.Err(); err != nil {
			return preparedImportPack{}, err
		}
		if _, duplicate := seenHashes[selection.Hash]; duplicate {
			return preparedImportPack{}, fmt.Errorf("%w %s across import selections", ErrDuplicateHash, selection.Hash)
		}
		seenHashes[selection.Hash] = struct{}{}
		entry := authoritative[selection.Hash]
		if entry.RawLen > uint64(limits.BlobBytes) || entry.StoredLen > uint64(limits.BlobBytes) {
			plan.fallbacks = append(plan.fallbacks, ImportFallback{
				PackID: candidate.PackID,
				Hash:   selection.Hash,
				Reason: FallbackBlobLimit,
			})
			continue
		}
		plan.selections = append(plan.selections, selection)
	}
	return plan, nil
}

func indexImportSelections(entries []pack.Entry, candidate ImportPack) (map[Hash]pack.Entry, error) {
	authoritative := make(map[Hash]pack.Entry, len(entries))
	for _, entry := range entries {
		hash, err := ParseHash(entry.ID.String())
		if err != nil {
			return nil, fmt.Errorf("prepare import pack %s footer: %w", candidate.PackID, err)
		}
		authoritative[hash] = entry
	}
	seen := make(map[Hash]struct{}, len(candidate.Selections))
	for _, selection := range candidate.Selections {
		if _, duplicate := seen[selection.Hash]; duplicate {
			return nil, fmt.Errorf("%w %s in import pack %s", ErrDuplicateHash, selection.Hash, candidate.PackID)
		}
		seen[selection.Hash] = struct{}{}
		entry, ok := authoritative[selection.Hash]
		if !ok {
			return nil, fmt.Errorf("%w: selected blob %s is absent from pack %s footer", pack.ErrCorrupt, selection.Hash, candidate.PackID)
		}
		if selection.RawLen != int64(entry.RawLen) || selection.Offset != entry.Offset ||
			selection.StoredLen != entry.StoredLen || selection.Flags != uint8(entry.Flags) {
			return nil, fmt.Errorf("%w: selected metadata for %s does not match pack %s footer", pack.ErrCorrupt, selection.Hash, candidate.PackID)
		}
	}
	return authoritative, nil
}
