package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

type restoreAttachmentInventory struct {
	chunked []ContentRef
	refs    []ContentRef
	paths   map[string][]string
	groups  map[string][]ContentRef
	order   []string
}

type packedRestoreResult struct {
	totalBlobs  int64
	totalBytes  int64
	packedBlobs int64
	looseBlobs  int64
	packs       int
	fallbacks   []packstore.ImportFallback
}

var (
	openStagedCatalogFile = func(root *os.Root, name string) (*os.File, error) {
		return root.OpenFile(name, os.O_RDWR, 0)
	}
	syncStagedCatalogFile  = func(file *os.File) error { return file.Sync() }
	closeStagedCatalogFile = func(file *os.File) error { return file.Close() }
)

// loadRestoreAttachmentInventory validates the snapshot lists, restored DB
// membership, restore paths, and repository index as one shared preflight for
// both the legacy loose writer and the optional mixed writer.
func (s *restoreState) loadRestoreAttachmentInventory(
	ctx context.Context, app App, m *Manifest,
) (restoreAttachmentInventory, error) {
	refs, _, err := LoadListRefs(s.repo, s.known, m.Attachments.Lists, nil, app.PackFileExtension())
	if err != nil {
		return restoreAttachmentInventory{}, err
	}
	if int64(len(refs)) != m.Attachments.Blobs {
		return restoreAttachmentInventory{}, fmt.Errorf(
			"backup: attachment lists name %d blobs but manifest reports %d", len(refs), m.Attachments.Blobs)
	}
	paths, err := s.restoredContentPaths(ctx)
	if err != nil {
		return restoreAttachmentInventory{}, err
	}
	listed := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		listed[ref.Hash] = struct{}{}
		if len(paths[ref.Hash]) == 0 {
			return restoreAttachmentInventory{}, fmt.Errorf(
				"backup: attachment blob %s is in the snapshot's lists but the restored database records no path for it",
				ref.Hash)
		}
	}
	for hash := range paths {
		if _, ok := listed[hash]; !ok {
			return restoreAttachmentInventory{}, fmt.Errorf(
				"backup: restored database references attachment %s that appears in no attachment list", hash)
		}
	}
	pathOwner := make(map[string]string)
	for hash, rels := range paths {
		if s.compression.Enabled && (len(rels) != 1 || filepath.ToSlash(rels[0]) != hash[:2]+"/"+hash) {
			return restoreAttachmentInventory{}, fmt.Errorf("backup: loose compression requires one canonical restore path for %s", hash)
		}
		for _, rel := range rels {
			if !filepath.IsLocal(rel) {
				return restoreAttachmentInventory{}, fmt.Errorf(
					"backup: attachment %s restore path %q escapes the content directory", hash, rel)
			}
			if bad := trailingDotOrSpaceComponent(rel); bad != "" {
				return restoreAttachmentInventory{}, fmt.Errorf(
					"backup: attachment %s restore path %q has component %q ending in a dot or space", hash, rel, bad)
			}
			key := foldedPathKey(rel)
			if other, ok := pathOwner[key]; ok && other != hash {
				return restoreAttachmentInventory{}, fmt.Errorf(
					"backup: restore path %q collides under case-folded key %q with two different attachments %s and %s",
					rel, key, other, hash)
			}
			pathOwner[key] = hash
		}
	}
	groups := make(map[string][]ContentRef)
	var order []string
	var chunked []ContentRef
	for _, ref := range refs {
		id, err := pack.ParseBlobID(ref.Hash)
		if err != nil {
			return restoreAttachmentInventory{}, fmt.Errorf("backup: attachment content hash %q: %w", ref.Hash, err)
		}
		if _, ok := s.recipes[id]; ok {
			chunked = append(chunked, ref)
			continue
		}
		entry, ok := s.known[id]
		if !ok {
			return restoreAttachmentInventory{}, fmt.Errorf("backup: attachment blob %s not present in any index", ref.Hash)
		}
		if _, seen := groups[entry.PackID]; !seen {
			order = append(order, entry.PackID)
		}
		groups[entry.PackID] = append(groups[entry.PackID], ref)
	}
	return restoreAttachmentInventory{refs: refs, paths: paths, groups: groups, order: order, chunked: chunked}, nil
}

func (s *restoreState) restorePackedAttachments(
	ctx context.Context,
	app App,
	m *Manifest,
	contentDir string,
	target PackedContentTarget,
	createdAt time.Time,
) (packedRestoreResult, error) {
	limits := target.Limits()
	if limits.BlobBytes < 0 || limits.PackBytes <= 0 || limits.FooterBytes <= 0 || limits.PackEntries <= 0 {
		return packedRestoreResult{}, errors.New("backup: packed content target returned invalid limits")
	}
	inventory, err := s.loadRestoreAttachmentInventory(ctx, app, m)
	if err != nil {
		return packedRestoreResult{}, err
	}
	result := packedRestoreResult{totalBlobs: int64(len(inventory.refs))}
	seenHashes := make(map[string]struct{}, len(inventory.refs))
	for _, ref := range inventory.refs {
		if _, duplicate := seenHashes[ref.Hash]; duplicate {
			return packedRestoreResult{}, fmt.Errorf(
				"backup: packed restore attachment lists name content hash %s more than once", ref.Hash)
		}
		seenHashes[ref.Hash] = struct{}{}
		if ref.Size > 0 && result.totalBytes > math.MaxInt64-ref.Size {
			return packedRestoreResult{}, errors.New("backup: attachment byte total overflows")
		}
		result.totalBytes += ref.Size
	}
	if result.totalBytes != m.Attachments.BlobBytes {
		return packedRestoreResult{}, fmt.Errorf(
			"backup: attachment lists sum to %d bytes but manifest reports %d",
			result.totalBytes, m.Attachments.BlobBytes)
	}
	if err := validatePackedRestorePaths(inventory.paths); err != nil {
		return packedRestoreResult{}, err
	}
	s.done, s.doneByte = 0, 0
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Total: result.totalBlobs, BytesTotal: result.totalBytes,
	})

	var candidates []packstore.ImportPack
	if app.PackFileExtension() == packstore.PackExt {
		candidates, err = s.importCandidates(inventory, app.PackFileExtension())
		if err != nil {
			return packedRestoreResult{}, err
		}
	}
	prepared, err := packstore.PrepareImport(ctx, s.root, filepath.ToSlash(contentDir), candidates, packstore.ImportOptions{
		Limits: limits, CreatedAt: createdAt.UTC(),
	})
	if err != nil {
		return packedRestoreResult{}, fmt.Errorf("backup: preparing packed attachment restore: %w", err)
	}
	stats := prepared.Stats()
	if app.PackFileExtension() != packstore.PackExt {
		for _, packID := range inventory.order {
			stats.Fallbacks = append(stats.Fallbacks, packstore.ImportFallback{
				PackID: packID, Reason: packstore.FallbackPackEncoding,
			})
		}
	}
	packedSet := make(map[string]struct{}, stats.PackedBlobs)
	for _, hash := range prepared.PackedHashes() {
		packedSet[hash.String()] = struct{}{}
	}
	if len(packedSet) != stats.PackedBlobs {
		return packedRestoreResult{}, errors.New("backup: packed import reported inconsistent selected hash count")
	}

	looseGroups := make(map[string][]ContentRef)
	var looseOrder []string
	for _, packID := range inventory.order {
		for _, ref := range inventory.groups[packID] {
			if _, packed := packedSet[ref.Hash]; packed {
				s.done++
				s.doneByte += ref.Size
				continue
			}
			if len(looseGroups[packID]) == 0 {
				looseOrder = append(looseOrder, packID)
			}
			looseGroups[packID] = append(looseGroups[packID], ref)
		}
	}
	if s.done > 0 {
		s.progress.emit(ProgressEvent{
			Stage: ProgressStageAttachments, Done: s.done, Total: result.totalBlobs,
			BytesDone: s.doneByte, BytesTotal: result.totalBytes,
		})
	}
	if err := s.runPackGroups(ctx, looseOrder, func(packID string) {
		s.restorePackAttachments(ctx, contentDir, packID, looseGroups[packID], inventory.paths, result.totalBlobs, result.totalBytes)
	}); err != nil {
		return packedRestoreResult{}, err
	}
	if err := s.restoreChunkedAttachments(ctx, contentDir, inventory, result.totalBytes); err != nil {
		return packedRestoreResult{}, err
	}
	result.packedBlobs = int64(len(packedSet))
	result.looseBlobs = result.totalBlobs - result.packedBlobs
	if result.packedBlobs < 0 || result.looseBlobs < 0 || result.packedBlobs+result.looseBlobs != result.totalBlobs {
		return packedRestoreResult{}, errors.New("backup: packed and loose attachment coverage is inconsistent")
	}
	if result.looseBlobs > 0 {
		if err := s.syncPackedRestoreContent(); err != nil {
			return packedRestoreResult{}, err
		}
	}
	if err := s.commitPreparedImport(ctx, target, prepared); err != nil {
		return packedRestoreResult{}, err
	}
	result.packs = stats.PackedPacks
	result.fallbacks = append([]packstore.ImportFallback(nil), stats.Fallbacks...)
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Done: result.totalBlobs, Total: result.totalBlobs,
		BytesDone: result.totalBytes, BytesTotal: result.totalBytes, Final: true,
	})
	return result, nil
}

func validatePackedRestorePaths(paths map[string][]string) error {
	for hash, rels := range paths {
		for _, rel := range rels {
			portable := path.Clean(strings.ReplaceAll(filepath.ToSlash(rel), `\`, "/"))
			first, _, _ := strings.Cut(portable, "/")
			if strings.EqualFold(first, "packs") {
				return fmt.Errorf(
					"backup: attachment %s restore path %q uses reserved packed-content subtree %q",
					hash, rel, "packs")
			}
		}
	}
	return nil
}

func (s *restoreState) syncPackedRestoreContent() error {
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	// Mixed restores deliberately use the existing complete-tree durability
	// pass. The final pass still runs after extras and database publication, so
	// mixed restores currently pay one additional directory traversal in exchange
	// for a simple, identical durability contract before catalog authority.
	if err := syncRestoredTree(s.target, s.syncCeiling); err != nil {
		return fmt.Errorf("backup: syncing loose attachment fallbacks before packed catalog authority: %w", err)
	}
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	return nil
}

func (s *restoreState) importCandidates(
	inventory restoreAttachmentInventory, ext string,
) ([]packstore.ImportPack, error) {
	candidates := make([]packstore.ImportPack, 0, len(inventory.order))
	for _, packID := range inventory.order {
		candidate := packstore.ImportPack{
			PackID: packID, SourcePath: s.repo.packPath(packID, ext),
			Selections: make([]packstore.ImportSelection, 0, len(inventory.groups[packID])),
		}
		for _, ref := range inventory.groups[packID] {
			hash, err := packstore.ParseHash(ref.Hash)
			if err != nil {
				return nil, fmt.Errorf("backup: attachment content hash %q: %w", ref.Hash, err)
			}
			id, _ := pack.ParseBlobID(ref.Hash)
			entry := s.known[id]
			candidate.Selections = append(candidate.Selections, packstore.ImportSelection{
				Hash: hash, RawLen: ref.Size, Offset: entry.Offset,
				StoredLen: entry.StoredLen, Flags: uint8(entry.Flags),
			})
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func (s *restoreState) commitPreparedImport(
	ctx context.Context, target PackedContentTarget, prepared *packstore.PreparedImport,
) (resultErr error) {
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	db, err := sqliteOpener(s.sqlite).OpenSQLite(filepath.Join(s.target, s.dbRead), SQLiteOpenOptions{
		Access: SQLiteReadWriteExisting, BusyTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("backup: opening staged database for packed content catalog: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if db != nil {
			resultErr = errors.Join(resultErr, s.closeStagedCatalogDB(db))
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("backup: opening staged database for packed content catalog: %w", err)
	}
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&journalMode); err != nil {
		return fmt.Errorf("backup: selecting staged catalog journal mode: %w", err)
	}
	if journalMode != "delete" {
		return fmt.Errorf("backup: staged catalog journal mode is %q, want delete", journalMode)
	}
	catalog, err := target.OpenRestoreCatalog(ctx, db)
	if err != nil {
		return fmt.Errorf("backup: opening packed content restore catalog: %w", err)
	}
	if catalog == nil {
		return errors.New("backup: packed content target returned a nil restore catalog")
	}
	if err := prepared.Commit(ctx, catalog); err != nil {
		return err
	}
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	if err := s.closeStagedCatalogDB(db); err != nil {
		db = nil
		return err
	}
	db = nil
	if err := s.syncAndCloseStagedCatalog(); err != nil {
		return err
	}
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	return nil
}

func (s *restoreState) syncAndCloseStagedCatalog() (resultErr error) {
	before, err := s.root.Lstat(s.dbRead)
	if err != nil {
		return fmt.Errorf("backup: inspecting staged packed content catalog before sync: %w", err)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf(
			"backup: staged packed content catalog %s is not a regular file before sync", s.dbRead)
	}

	file, err := openStagedCatalogFile(s.root, s.dbRead)
	if err != nil {
		return fmt.Errorf("backup: opening staged packed content catalog for sync: %w", err)
	}
	defer func() {
		if err := closeStagedCatalogFile(file); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf(
				"backup: closing synced staged packed content catalog: %w", err))
		}
	}()

	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("backup: inspecting opened staged packed content catalog before sync: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return fmt.Errorf(
			"backup: opened staged packed content catalog identity does not match %s before sync", s.dbRead)
	}
	if err := syncStagedCatalogFile(file); err != nil {
		return fmt.Errorf("backup: syncing staged packed content catalog: %w", err)
	}

	after, err := s.root.Lstat(s.dbRead)
	if err != nil {
		return fmt.Errorf("backup: inspecting staged packed content catalog after sync: %w", err)
	}
	if !after.Mode().IsRegular() {
		return fmt.Errorf(
			"backup: staged packed content catalog %s is not a regular file after sync", s.dbRead)
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return fmt.Errorf("backup: inspecting opened staged packed content catalog after sync: %w", err)
	}
	if !openedAfter.Mode().IsRegular() || !os.SameFile(after, openedAfter) {
		return fmt.Errorf(
			"backup: staged packed content catalog identity changed during sync for %s", s.dbRead)
	}
	return nil
}

func (s *restoreState) closeStagedCatalogDB(db *sql.DB) error {
	if err := db.Close(); err != nil {
		return fmt.Errorf(
			"backup: closing staged packed content catalog before sidecar cleanup: %w", err)
	}
	var cleanupErr error
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		name := s.dbRead + suffix
		if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("backup: removing staged packed content catalog sidecar %s: %w", name, err))
		}
	}
	return cleanupErr
}
