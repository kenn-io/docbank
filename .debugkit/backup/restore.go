package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"

	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/pathresolve"
	"go.kenn.io/kit/safefileio"
)

// RestoreOptions parameterizes one restore run (FORMAT.md, Restore).
type RestoreOptions struct {
	SnapshotID string // empty: latest
	// TargetDir receives the restored archive: the database file, content dir,
	// and any captured extras. It must not exist, or be an empty directory,
	// unless Overwrite is set. Overwrite merges into the existing tree:
	// restored files replace same-named ones, files the snapshot does not
	// carry are left in place, and the existing database and its SQLite
	// sidecars survive until the replacement database is fully materialized,
	// every attachment and extras blob has been read and verified, and the
	// replacement has passed the restore proof — only then are the sidecars
	// set aside (a stale -wal, -shm, or -journal would otherwise be replayed
	// over the restored file on its first normal open), the database renamed
	// into place, and the set-aside sidecars removed. A failed rename puts
	// the sidecars back.
	TargetDir string
	Overwrite bool
	// Jobs is the number of concurrent pack-read workers. Zero or negative
	// selects one per CPU. Use 1 to read packs strictly one at a time when
	// the repository lives on a spinning disk or NAS share.
	Jobs        int
	ForceUnlock bool
	// SkipIntegrityCheck omits SQLite's full PRAGMA integrity_check after
	// materialization. Database pages and content blobs remain SHA-256 verified,
	// and restored application statistics are still compared with the manifest.
	SkipIntegrityCheck bool
	// Progress, if non-nil, receives structured progress events as Restore
	// runs. nil means fully silent.
	Progress func(ProgressEvent)
	// MetadataRestorer is required for snapshots whose application state is a
	// portable metadata artifact instead of SQLite page maps.
	MetadataRestorer MetadataRestorer
	// PackedContent optionally restores compatible repository packs into the
	// target content store and replaces packed authority in the unpublished
	// staged DB before its integrity/stats proof and final publication. Pack and
	// entry compatibility use the target's configured limits; declined hashes
	// are restored and verified loose. nil preserves a fully-loose restore.
	// When non-nil, "packs" is reserved as the first component below the
	// application's content directory.
	PackedContent PackedContentTarget
	// LooseCompression applies Kit's canonical loose encoding to content not
	// imported as packs. It requires canonical <hash[:2]>/<hash> restore paths
	// and BeforePublication to record the returned LooseContent receipts in the
	// staged catalog. Extras and the database remain ordinary files.
	LooseCompression packstore.LooseCompressionOptions
	// TargetCoordinator optionally acquires application-owned coordination
	// against the exact pre-opened target root Restore will mutate. This closes
	// the pathname gap that would exist if a caller locked TargetDir before Kit
	// opened it. The lease is held through publication and durability sync.
	TargetCoordinator RestoreTargetCoordinator
	// SQLiteOpener selects the SQLite implementation used to validate and
	// update the staged database. Nil preserves Kit's mattn/go-sqlite3 default.
	SQLiteOpener SQLiteOpener
	// AuxiliaryTarget stages verified application-defined snapshot artifacts
	// after all restore proofs and commits them only after the target is
	// published and durably synced.
	AuxiliaryTarget AuxiliaryTarget
	// BeforePublication runs after every content object has been restored and
	// verified but before the staged database's integrity/stats proof and
	// canonical publication. Applications may update a private scratch copy to
	// bind restored physical state; DBPath is outside TargetDir and does not
	// resolve through it. The callback must close/checkpoint its database and
	// leave no SQLite sidecars; returning an error leaves it unpublished.
	BeforePublication func(context.Context, RestorePublicationTarget) error
}

// RestorePublicationTarget identifies the private scratch state passed to
// BeforePublication. DBPath is outside TargetDir and valid only for the
// duration of the callback.
type RestorePublicationTarget struct {
	TargetDir string
	DBPath    string
	// LooseContent reports verified physical representations when
	// LooseCompression is enabled, including objects retained without compression.
	LooseContent []packstore.WriteResult
}

// RestoreResult reports what Restore materialized and proved.
type RestoreResult struct {
	SnapshotID      string
	DBPath          string
	DBBytes         int64
	AttachmentBlobs int64
	AttachmentBytes int64
	// PackedAttachmentBlobs and LooseAttachmentBlobs partition
	// AttachmentBlobs by restored representation.
	PackedAttachmentBlobs int64
	LooseAttachmentBlobs  int64
	// AttachmentPacks is the number of repository packs imported and granted
	// authority in the restored application catalog.
	AttachmentPacks int
	// PackFallbacks records why packs or individual selected hashes were
	// restored loose. An empty Hash means the reason applies to the whole pack.
	PackFallbacks []packstore.ImportFallback
	ExtrasFiles   int
	// AuxiliaryArtifacts is the number of verified artifacts committed through
	// AuxiliaryTarget.
	AuxiliaryArtifacts int
	// DatabaseIntegrityChecked reports whether Restore ran SQLite's full
	// PRAGMA integrity_check against the staged database.
	DatabaseIntegrityChecked bool
	Duration                 time.Duration
}

// Restore materializes one snapshot into TargetDir and then proves the
// result (FORMAT.md, Restore): every database page
// is hash-verified against the snapshot's page-hash map as it is written,
// every blob read re-derives its SHA-256 identity, and the restored database
// reproduces the manifest's recorded stats exactly before Restore reports
// success, and passes PRAGMA integrity_check unless SkipIntegrityCheck is set.
// When PackedContent is set,
// compatible packs are durably published before one staged catalog replacement;
// every fallback is durably restored loose before that authority change.
//
// It takes a SHARED repository lock: concurrent restores and verifies are
// safe, a running create is not.
func Restore(ctx context.Context, r *Repo, app App, opts RestoreOptions) (res *RestoreResult, err error) {
	start := time.Now()
	if err := validatePackExtension(app.PackFileExtension()); err != nil {
		return nil, err
	}
	if opts.TargetDir == "" {
		return nil, errors.New("backup: restore target directory is required")
	}
	if opts.LooseCompression.Enabled && opts.BeforePublication == nil {
		return nil, errors.New("backup: loose compression requires BeforePublication to record physical content")
	}
	if opts.LooseCompression.MinBytes < 0 || opts.LooseCompression.MinSavingsPercent < 0 || opts.LooseCompression.MinSavingsPercent > 100 {
		return nil, errors.New("backup: invalid loose compression policy")
	}
	// Normalize the target once, before anything resolves it: with a trailing
	// separator ("link/", "link/.") POSIX resolves a final-component symlink
	// during lstat, so openRestoreRoot's leaf check — and verifyHeldTarget's
	// re-checks — would pass on a symlinked target addressed that way and
	// redirect every restore write into the link's destination. Cleaning here
	// also keeps every downstream consumer of the path (sync ceiling, DBPath,
	// the held-target re-verification) on one canonical spelling.
	opts.TargetDir = filepath.Clean(opts.TargetDir)
	lock, err := r.AcquireSharedLock("restore", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	var m *Manifest
	if opts.SnapshotID != "" {
		if m, err = r.LoadManifest(opts.SnapshotID); err != nil {
			return nil, err
		}
	} else {
		if m, err = r.LatestSnapshot(); err != nil {
			return nil, err
		}
		if m == nil {
			return nil, errors.New("backup: repository has no snapshots to restore")
		}
	}
	if len(m.Auxiliary) > 0 && opts.AuxiliaryTarget == nil {
		return nil, errors.New("backup: snapshot carries auxiliary artifacts but no AuxiliaryTarget was provided")
	}
	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}
	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = runtime.GOMAXPROCS(0)
	}
	st := &restoreState{
		repo:        r,
		app:         app,
		known:       known,
		jobs:        jobs,
		progress:    newProgressEmitter(opts.Progress),
		target:      opts.TargetDir,
		sqlite:      sqliteOpener(opts.SQLiteOpener),
		compression: opts.LooseCompression,
	}

	// Source preflight runs BEFORE the target is touched: materializing the
	// maps proves the manifest's chains resolve and decode, and the blob
	// pass proves every content blob the snapshot references resolves
	// through the index and that the extras tree's paths are restorable, so
	// a corrupt repository or missing index fails here. Failures the index
	// cannot reveal (unreadable pack bytes) are covered by ordering instead:
	// the target's database and sidecars are touched only after every
	// content blob — pages, attachments, extras — has been read and
	// verified (publishRestoredDB).
	var hm *PageHashMap
	var pm *PageMap
	if m.Metadata == nil {
		hm, pm, err = st.materializeMaps(m)
		if err != nil {
			return nil, err
		}
	} else if opts.MetadataRestorer == nil {
		return nil, errors.New("backup: portable metadata snapshot requires a MetadataRestorer")
	}
	if err := st.preflightSnapshotBlobs(ctx, m, pm); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The ceiling must be observed BEFORE prepareRestoreTarget creates the
	// target: it marks the deepest directory that already existed, so the
	// final durability pass knows which ancestors gained new entries.
	syncCeiling := restoreSyncCeiling(opts.TargetDir)
	st.syncCeiling = syncCeiling
	root, existed, err := openRestoreTarget(opts.TargetDir, opts.Overwrite)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var targetLease RestoreTargetLease
	releaseTargetLease := func() error {
		if targetLease == nil {
			return nil
		}
		lease := targetLease
		targetLease = nil
		if releaseErr := lease.Release(); releaseErr != nil {
			return fmt.Errorf("backup: releasing restore target coordination: %w", releaseErr)
		}
		return nil
	}
	defer func() { err = errors.Join(err, releaseTargetLease()) }()
	if opts.TargetCoordinator != nil {
		var acquireErr error
		targetLease, acquireErr = opts.TargetCoordinator.AcquireRestoreTarget(ctx, root)
		if acquireErr != nil {
			return nil, fmt.Errorf("backup: acquiring restore target coordination: %w", acquireErr)
		}
		if targetLease == nil {
			return nil, errors.New("backup: restore target coordinator returned a nil lease")
		}
		if err := verifyRestoreRoot(opts.TargetDir, root); err != nil {
			return nil, err
		}
	}
	// The pathname-level check in openRestoreTarget is only an early refusal.
	// Coordination can block while another owner still holds the target, so
	// enforce the caller's non-overwrite policy again against the pinned root
	// after the lease makes this decision authoritative.
	if !opts.Overwrite {
		entries, readErr := fs.ReadDir(root.FS(), ".")
		if readErr != nil {
			return nil, fmt.Errorf("backup: reading coordinated restore target: %w", readErr)
		}
		if len(entries) > 0 {
			return nil, fmt.Errorf(
				"backup: restore target %s is not empty (use --overwrite to restore into it anyway)",
				opts.TargetDir)
		}
	}
	if existed {
		if err := removeRestoreTempFiles(root, app.DBFileName()); err != nil {
			return nil, err
		}
	}
	st.root = root

	res = &RestoreResult{
		SnapshotID: m.SnapshotID,
		DBPath:     filepath.Join(opts.TargetDir, app.DBFileName()),
	}
	if pm != nil {
		res.DBBytes = int64(pm.PageCount * uint64(pm.PageSize))
	}
	// The database stays in its staging temp until attachments and extras
	// have fully materialized: every one of those reads re-derives its
	// blob's SHA-256, so an unreadable or corrupt content pack — which the
	// index-membership preflight cannot see — fails the restore before
	// publishRestoredDB touches an Overwrite target's existing database.
	var tmpRel string
	if m.Metadata == nil {
		if pm == nil {
			return nil, errors.New("backup: snapshot has no page map")
		}
		tmpRel, err = st.restoreDB(ctx, app.DBFileName(), pm, hm)
	} else {
		tmpRel, res.DBBytes, err = st.restorePortableMetadata(
			ctx, app.DBFileName(), m.Metadata, opts.MetadataRestorer)
	}
	if err != nil {
		return nil, err
	}
	st.dbRead = tmpRel
	published := false
	defer func() {
		if !published {
			_ = st.root.Remove(tmpRel)
		}
	}()
	var auxiliary []RestoredAuxiliary
	if len(m.Auxiliary) > 0 {
		auxiliary, err = st.restoreAuxiliary(ctx, m)
		if err != nil {
			return nil, err
		}
	}
	var packedRestoreLease *packstore.Lease
	releasePackedRestoreLease := func() error {
		if packedRestoreLease == nil {
			return nil
		}
		lease := packedRestoreLease
		packedRestoreLease = nil
		if releaseErr := lease.Release(); releaseErr != nil {
			return fmt.Errorf("backup: releasing packed restore lease: %w", releaseErr)
		}
		return nil
	}
	defer func() { err = errors.Join(err, releasePackedRestoreLease()) }()
	if opts.PackedContent == nil {
		res.AttachmentBlobs, res.AttachmentBytes, err = st.restoreAttachments(
			ctx, app, m, app.ContentDirName())
		res.LooseAttachmentBlobs = res.AttachmentBlobs
	} else {
		var acquireErr error
		packedRestoreLease, acquireErr = opts.PackedContent.AcquireRestoreLease(ctx)
		if acquireErr != nil {
			return nil, fmt.Errorf("backup: acquiring packed restore lease: %w", acquireErr)
		}
		if validateErr := packedRestoreLease.ValidateMutation(); validateErr != nil {
			return nil, fmt.Errorf("backup: validating packed restore mutation lease: %w", validateErr)
		}
		var packed packedRestoreResult
		packed, err = st.restorePackedAttachments(ctx, app, m, app.ContentDirName(), opts.PackedContent, start)
		res.AttachmentBlobs = packed.totalBlobs
		res.AttachmentBytes = packed.totalBytes
		res.PackedAttachmentBlobs = packed.packedBlobs
		res.LooseAttachmentBlobs = packed.looseBlobs
		res.AttachmentPacks = packed.packs
		res.PackFallbacks = append([]packstore.ImportFallback(nil), packed.fallbacks...)
	}
	if err != nil {
		return nil, err
	}
	// Extras are staged, not published: each entry's blob is fetched,
	// hash-verified, and written to a temp sibling of its final path, but
	// the temps are renamed into place only after the proof passes, just
	// before the database publish. Extras overwrite live operational files
	// in an Overwrite target, so any failure up to and including the proof
	// must leave every one of them untouched — unlike attachments, whose
	// content-addressed paths make partial writes benign.
	extras, err := st.stageExtras(ctx, app, m)
	if err != nil {
		return nil, err
	}
	res.ExtrasFiles = len(extras)
	defer func() {
		if !published {
			st.removeStagedFiles(extras)
		}
	}()
	if opts.BeforePublication != nil {
		replacementRel, replacementBytes, prepareErr := st.prepareBeforePublication(
			ctx, tmpRel, app.DBFileName(), opts.BeforePublication)
		if prepareErr != nil {
			return nil, prepareErr
		}
		tmpRel = replacementRel
		res.DBBytes = replacementBytes
		st.dbRead = replacementRel
	}

	// The proof runs against the staging temp, BEFORE the database is
	// published: an enabled integrity_check failure, a stats mismatch, or a late
	// cancellation must fail the restore while an Overwrite target's
	// existing database is still intact. Report the optional SQLite scan and
	// the inexpensive manifest-statistics comparison as distinct stages.
	startStats := func() {
		st.progress.emit(ProgressEvent{Stage: ProgressStageRestoreStats, Total: 1})
	}
	if !opts.SkipIntegrityCheck {
		st.progress.emit(ProgressEvent{Stage: ProgressStageIntegrityCheck, Total: 1})
	} else {
		startStats()
	}
	if err := st.proveRestoredDB(ctx, m, !opts.SkipIntegrityCheck, func() {
		res.DatabaseIntegrityChecked = true
		st.progress.emit(ProgressEvent{
			Stage: ProgressStageIntegrityCheck, Done: 1, Total: 1, Final: true,
		})
		startStats()
	}); err != nil {
		return nil, err
	}
	st.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreStats, Done: 1, Total: 1, Final: true,
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var stagedAuxiliary AuxiliaryRestore
	if len(auxiliary) > 0 {
		stagedAuxiliary, err = opts.AuxiliaryTarget.StageAuxiliary(ctx, auxiliary)
		if err != nil {
			return nil, fmt.Errorf("backup: staging auxiliary artifacts: %w", err)
		}
		if stagedAuxiliary == nil {
			return nil, errors.New("backup: auxiliary target returned a nil restore transaction")
		}
		defer func() {
			if stagedAuxiliary == nil {
				return
			}
			rollbackCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), auxiliaryRollbackTimeout,
			)
			rollbackErr := stagedAuxiliary.Rollback(rollbackCtx)
			cancel()
			if rollbackErr != nil {
				err = errors.Join(err,
					fmt.Errorf("backup: rolling back auxiliary artifacts: %w", rollbackErr))
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := st.promoteExtras(extras); err != nil {
		return nil, err
	}
	if err := st.publishRestoredDB(tmpRel, app.DBFileName()); err != nil {
		return nil, err
	}
	published = true

	if err := syncRestoredTree(opts.TargetDir, syncCeiling); err != nil {
		return nil, err
	}
	if err := st.removeLooseAlternates(ctx); err != nil {
		return res, err
	}
	if err := releasePackedRestoreLease(); err != nil {
		return res, err
	}
	if err := releaseTargetLease(); err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stagedAuxiliary != nil {
		if err := stagedAuxiliary.Commit(ctx); err != nil {
			return nil, fmt.Errorf("backup: committing auxiliary artifacts: %w", err)
		}
		stagedAuxiliary = nil
		res.AuxiliaryArtifacts = len(auxiliary)
	}
	res.Duration = time.Since(start)
	return res, nil
}

// restoreSyncCeiling returns the deepest ancestor of target that already
// exists — or target itself when it does. Every directory restore creates
// below the ceiling (the target and any missing ancestors os.MkdirAll fills
// in) adds a directory entry that syncRestoredTree must make durable, and
// the ceiling itself receives the topmost new entry.
func restoreSyncCeiling(target string) string {
	p := target
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// syncRestoredTree fsyncs every directory under target, deepest first, then
// upward from target's parent through ceiling, so the directory ENTRIES of
// everything restore created — nested attachment fan-out directories, extras
// subtrees, the target itself and any ancestors created for it — are as
// durable as the file contents by the time Restore reports success.
// writeRestoredFile fsyncs each file's bytes but not the directories naming
// them; without this pass a crash shortly after a successful restore could
// lose newly created paths. One sync per directory at the end costs far less
// than fsyncing parents on every file write, and the guarantee only needs to
// hold at success.
func syncRestoredTree(target, ceiling string) error {
	var dirs []string
	err := filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("backup: walking restore target for directory sync: %w", err)
	}
	// The walk visits parents before their children; going backward yields
	// every directory after all its descendants, so each entry is durable
	// in its parent by the time that parent is synced.
	for _, dir := range slices.Backward(dirs) {
		if err := pack.SyncDir(dir); err != nil {
			return fmt.Errorf("backup: syncing restored directory: %w", err)
		}
	}
	// Ancestors restore created above target, and the pre-existing ceiling
	// directory that received the topmost new entry, sit outside the walk;
	// climbing from target's parent continues the deepest-first order. When
	// the ceiling is the target itself, the target predates this restore
	// and nothing above it changed.
	if ceiling == target {
		return nil
	}
	for p := filepath.Dir(target); ; p = filepath.Dir(p) {
		if err := pack.SyncDir(p); err != nil {
			return fmt.Errorf("backup: syncing restored directory: %w", err)
		}
		if p == ceiling || p == filepath.Dir(p) {
			return nil
		}
	}
}

// sqliteSidecarNames lists the sidecar files SQLite may create next to a
// database: the WAL and its shared-memory index, and the rollback journal. A
// stale sidecar next to a restored database would be replayed or reused on
// the file's first normal open, silently altering the proven bytes — so
// overwrite restores set them aside before publishing the database
// (publishRestoredDB) and extras entries may never plant one.
func sqliteSidecarNames(dbFileName string) []string {
	return []string{dbFileName + "-wal", dbFileName + "-shm", dbFileName + "-journal"}
}

// setAsideDBSidecars renames each SQLite sidecar file sitting next to the
// target database to a single-run staging name (<sidecar>.restore-<ulid>)
// and returns original name → aside name for the ones that existed. Renaming
// rather than removing keeps publication rollbackable: a publish that then
// fails renames the asides back (putBackDBSidecars), so the target's live
// database never loses the -wal frames or hot -journal it needs to open
// correctly to a restore that did not replace it. Renaming through the root
// moves a symlink planted at those names, never follows it.
func setAsideDBSidecars(root *os.Root, dbFileName string) (map[string]string, error) {
	asides := make(map[string]string)
	for _, name := range sqliteSidecarNames(dbFileName) {
		aside := name + ".restore-" + pack.NewPackID()
		if err := root.Rename(name, aside); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return asides, fmt.Errorf(
				"backup: setting aside stale %s in restore target: %w", name, err)
		}
		asides[name] = aside
	}
	return asides, nil
}

// putBackDBSidecars renames set-aside sidecar files back to their original
// names after a failed publish, joining every failure so the caller reports
// exactly which files did not come back and where their bytes remain.
func putBackDBSidecars(root *os.Root, asides map[string]string) error {
	var errs []error
	for name, aside := range asides {
		if err := root.Rename(aside, name); err != nil {
			errs = append(errs, fmt.Errorf(
				"backup: restoring set-aside %s (bytes remain at %s): %w", name, aside, err))
		}
	}
	return errors.Join(errs...)
}

// openRestoreTarget creates TargetDir, refusing a non-empty existing directory
// unless overwrite is set (FORMAT.md, Restore), and returns a root confined to
// it plus whether the directory already existed. Cleanup is deliberately
// separate so application coordination can be acquired against this exact root
// before any existing target entry is mutated.
func openRestoreTarget(target string, overwrite bool) (*os.Root, bool, error) {
	if target == "" {
		return nil, false, errors.New("backup: restore target directory is required")
	}
	entries, err := os.ReadDir(target)
	existed := true
	switch {
	case errors.Is(err, os.ErrNotExist):
		existed = false
		root, createErr := openMissingRestoreTarget(
			target, enterRestoreDir, pathresolve.EvalSymlinks)
		if createErr != nil {
			return nil, false, createErr
		}
		return root, existed, nil
	case err != nil:
		return nil, false, fmt.Errorf("backup: reading restore target: %w", err)
	case len(entries) > 0 && !overwrite:
		return nil, false, fmt.Errorf("backup: restore target %s is not empty (use --overwrite to restore into it anyway)", target)
	}
	root, err := openRestoreRoot(target)
	if err != nil {
		return nil, false, err
	}
	return root, existed, nil
}

// openMissingRestoreTarget creates each absent component relative to a held
// parent root. Pathname-based MkdirAll followed by OpenRoot leaves a gap where
// replacing an ancestor can redirect creation before the target descriptor is
// acquired. The final identity check also proves the original spelling still
// names the descriptor assembled through the held chain.
func openMissingRestoreTarget(
	target string,
	enter func(*os.Root, string) (*os.Root, error),
	resolve func(string) (string, error),
) (*os.Root, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, fmt.Errorf("backup: resolving restore target: %w", err)
	}
	current := abs
	var missing []string
	for {
		_, err = os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("backup: checking restore target ancestor: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("backup: restore target has no existing ancestor: %s", target)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	ancestor, err := os.Stat(current)
	if err != nil {
		return nil, fmt.Errorf("backup: checking restore target ancestor identity: %w", err)
	}
	resolved, err := resolve(current)
	if err != nil {
		return nil, fmt.Errorf("backup: resolving restore target ancestor: %w", err)
	}
	root, err := openRestoreRoot(resolved)
	if err != nil {
		return nil, err
	}
	heldAncestor, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("backup: checking held restore target ancestor: %w", err)
	}
	if !os.SameFile(ancestor, heldAncestor) {
		_ = root.Close()
		return nil, fmt.Errorf(
			"backup: restore target ancestor %s was replaced while opening it", current)
	}
	slices.Reverse(missing)
	for _, comp := range missing {
		next, enterErr := enter(root, comp)
		_ = root.Close()
		if enterErr != nil {
			return nil, fmt.Errorf(
				"backup: creating restore target component %q: %w", comp, enterErr)
		}
		root = next
	}
	if err := verifyRestoreRoot(abs, root); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// prepareRestoreTarget retains the package's direct preparation helper for
// internal callers and tests. Restore itself splits opening from cleanup so an
// application coordinator can acquire the pinned target first.
func prepareRestoreTarget(target string, overwrite bool, dbFileName string) (*os.Root, error) {
	root, existed, err := openRestoreTarget(target, overwrite)
	if err != nil {
		return nil, err
	}
	if !existed {
		return root, nil
	}
	// Overwrite merges rather than clearing the tree; even the existing
	// database and its SQLite sidecars survive until every content blob has
	// been read and verified and publishRestoredDB swaps the replacement in,
	// so a source failure mid-restore leaves the live database usable.
	//
	// A restore killed mid-run leaves the database's staging temp
	// (<db>.restore-<ulid>, restoreDB) at the target root, and one killed
	// mid-publish leaves sidecar asides (<db>-wal.restore-<ulid>,
	// setAsideDBSidecars) too; a later overwrite rerun would otherwise
	// ignore them forever. Sweep only those exact top-level shapes — never
	// a broad glob.
	if err := removeRestoreTempFiles(root, dbFileName); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// removeRestoreTempFiles deletes the staging temp files a crashed restore
// may have left at the target's top level: the database staging temp
// (restoreDB) and sidecar asides (setAsideDBSidecars). The name shapes are
// exact — dbFileName or a sidecar name, then ".restore-" and a 26-char
// lowercase ULID. The sweep runs entirely through the already-verified root
// — never a fresh pathname resolution of target — so a symlink swapped in
// at target after openRestoreRoot proved it cannot redirect the list or the
// removals outside the restore tree.
func removeRestoreTempFiles(root *os.Root, dbFileName string) error {
	names := append([]string{dbFileName}, sqliteSidecarNames(dbFileName)...)
	prefixes := make([]string, 0, len(names))
	for _, name := range names {
		prefixes = append(prefixes, name+".restore-")
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("backup: scanning restore target for stale temp files: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		for _, prefix := range prefixes {
			id, ok := strings.CutPrefix(e.Name(), prefix)
			if !ok || !pack.IsValidPackID(id) {
				continue
			}
			if err := root.Remove(e.Name()); err != nil &&
				!errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("backup: removing stale restore temp %s: %w", e.Name(), err)
			}
			break
		}
	}
	return nil
}

// openRestoreRoot opens a root confined to the restore target directory and
// proves the descriptor it holds is the real (non-symlink) directory at
// target. os.OpenRoot follows a final-component symlink, so a link planted at
// the target — before this run, or raced in against prepareRestoreTarget's
// MkdirAll — would otherwise redirect the entire restore under its link
// target. Checking AFTER the open closes the check-then-open race: Lstat
// reports a swapped-in link itself, and SameFile ties the lstat'd directory
// to the descriptor the root actually holds. Ancestor symlinks stay allowed
// (the path is user-supplied and resolved once); only the leaf is verified.
func openRestoreRoot(target string) (*os.Root, error) {
	root, err := os.OpenRoot(target)
	if err != nil {
		return nil, fmt.Errorf("backup: opening restore target: %w", err)
	}
	if err := verifyRestoreRoot(target, root); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func verifyRestoreRoot(target string, root *os.Root) error {
	leaf, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("backup: checking restore target: %w", err)
	}
	if leaf.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"backup: restore target %s is a symlink; pass the real directory path", target)
	}
	held, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("backup: checking restore target: %w", err)
	}
	if !os.SameFile(leaf, held) {
		return fmt.Errorf(
			"backup: restore target %s was replaced while opening it", target)
	}
	return nil
}

// restoreState carries the shared read machinery for one Restore run. mu
// guards progress counters and the first-error slot while pack workers run.
type restoreState struct {
	compression  packstore.LooseCompressionOptions
	looseContent []packstore.WriteResult // protected by mu while pack workers run
	recipes      map[pack.BlobID]objectRecipe
	repo         *Repo
	app          App
	known        map[pack.BlobID]IndexEntry
	jobs         int
	progress     *progressEmitter
	// root confines every restore write beneath the verified target directory;
	// its methods refuse any path that escapes via symlink. target is the
	// caller-supplied path root was opened at; SQLite opens must go by path,
	// so verifyHeldTarget re-ties that path to root around each one.
	root   *os.Root
	target string
	sqlite SQLiteOpener
	// syncCeiling is the deepest target ancestor that predated this restore.
	// Packed restore uses it for the pre-authority durability barrier; the
	// final tree sync reuses it after extras and the database are published.
	syncCeiling string
	// dbRead is the relative name read-only database opens use: the staging
	// temp, which both content-path derivation (restoreAttachments) and the
	// restore proof (proveRestoredDB) read — the database is published only
	// after every read and the proof have succeeded, and nothing opens it
	// afterward.
	dbRead string

	mu       sync.Mutex
	firstErr error
	done     int64 // stage-local progress counter (pages, then files)
	doneByte int64
}

// fail records the run's first error; workers check failed() to stop early.
func (s *restoreState) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstErr == nil {
		s.firstErr = err
	}
}

func (s *restoreState) failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr != nil
}

// fetch reads one metadata blob through the repository index.
func (s *restoreState) fetch(id pack.BlobID) ([]byte, error) {
	return s.repo.ReadBlob(s.known, id, nil, s.app.PackFileExtension())
}

// materializeMaps walks and materializes the snapshot's hash-map and
// page-map chains, cross-checking both against the manifest's recorded
// geometry and the page map's coverage before any byte is written.
func (s *restoreState) materializeMaps(m *Manifest) (*PageHashMap, *PageMap, error) {
	hashChain, err := s.repo.HashMapChain(m)
	if err != nil {
		return nil, nil, err
	}
	hm, err := MaterializeHashMap(s.fetch, hashChain)
	if err != nil {
		return nil, nil, err
	}
	pageChain, err := s.repo.PageMapChain(m)
	if err != nil {
		return nil, nil, err
	}
	pm, err := MaterializePageMap(s.fetch, pageChain)
	if err != nil {
		return nil, nil, err
	}
	if err := pm.CheckCoverage(); err != nil {
		return nil, nil, err
	}
	if pm.PageCount != m.DB.PageCount || pm.PageSize != m.DB.PageSize {
		return nil, nil, fmt.Errorf(
			"backup: page map geometry (%d pages of %d bytes) disagrees with manifest (%d pages of %d bytes)",
			pm.PageCount, pm.PageSize, m.DB.PageCount, m.DB.PageSize)
	}
	if hm.PageCount != m.DB.PageCount || hm.PageSize != m.DB.PageSize {
		return nil, nil, fmt.Errorf(
			"backup: page hash map geometry (%d pages of %d bytes) disagrees with manifest (%d pages of %d bytes)",
			hm.PageCount, hm.PageSize, m.DB.PageCount, m.DB.PageSize)
	}
	return hm, pm, nil
}

// preflightSnapshotBlobs proves every content blob the snapshot references —
// page data, attachments, extras files — resolves through the blob index, and
// that the extras tree's paths obey the rules restore enforces, before restore
// touches the target. materializeMaps already proved the metadata chains
// decode; without this pass a repository missing a content blob would fail
// mid-restore, after the target's database has been replaced and its sidecars
// removed. Blob contents are not read here (the drain reads
// and hash-verifies them as they are written); the small metadata blobs the
// pass re-reads — attachment lists and the extras tree — are cheap next to
// the content reads restore performs anyway.
func (s *restoreState) preflightSnapshotBlobs(ctx context.Context, m *Manifest, pm *PageMap) error {
	var err error
	s.recipes, err = loadObjectRecipes(ctx, s.repo, s.known, m, s.app.PackFileExtension())
	if err != nil {
		return err
	}
	if pm != nil {
		for _, id := range pm.Blobs {
			if _, ok := s.known[id]; !ok {
				return fmt.Errorf("backup: page blob %s not present in any index", id)
			}
		}
	}
	if m.Metadata != nil {
		id, err := pack.ParseBlobID(m.Metadata.Blob)
		if err != nil {
			return fmt.Errorf("backup: portable metadata blob id %q: %w", m.Metadata.Blob, err)
		}
		if err := s.checkObject(id, m.Metadata.Bytes); err != nil {
			return fmt.Errorf("backup: portable metadata blob %s: %w", id, err)
		}
	}
	for _, artifact := range m.Auxiliary {
		id, err := pack.ParseBlobID(artifact.Blob)
		if err != nil {
			return fmt.Errorf(
				"backup: auxiliary artifact %q blob id %q: %w",
				artifact.Name, artifact.Blob, err,
			)
		}
		if _, ok := s.known[id]; !ok {
			return fmt.Errorf(
				"backup: auxiliary artifact %q blob %s not present in any index",
				artifact.Name, id,
			)
		}
	}
	refs, _, err := LoadListRefs(s.repo, s.known, m.Attachments.Lists, nil, s.app.PackFileExtension())
	if err != nil {
		return err
	}
	for _, ref := range refs {
		id, err := pack.ParseBlobID(ref.Hash)
		if err != nil {
			return fmt.Errorf("backup: attachment content hash %q: %w", ref.Hash, err)
		}
		if err := s.checkObject(id, ref.Size); err != nil {
			return fmt.Errorf("backup: attachment blob %s: %w", ref.Hash, err)
		}
	}
	if m.Extras.Tree == "" {
		return nil
	}
	treeID, err := pack.ParseBlobID(m.Extras.Tree)
	if err != nil {
		return fmt.Errorf("backup: extras tree blob id %q: %w", m.Extras.Tree, err)
	}
	raw, err := s.fetch(treeID)
	if err != nil {
		return err
	}
	var tree ExtrasTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return fmt.Errorf("backup: extras tree %s: %w", treeID, err)
	}
	// The tree is decoded here anyway, so the path rules stageExtrasEntry
	// enforces per entry run now too: a tampered tree whose paths escape,
	// collide, or overlap archive content must fail while the target is
	// still untouched, not after the database has been replaced.
	if err := checkExtrasCollisions(tree.Entries); err != nil {
		return err
	}
	for _, entry := range tree.Entries {
		if _, err := validateExtrasEntryPath(
			entry.Path, s.app.ContentDirName(), s.app.DBFileName()); err != nil {
			return err
		}
		id, err := pack.ParseBlobID(entry.Blob)
		if err != nil {
			return fmt.Errorf("backup: extras entry %s blob id %q: %w", entry.Path, entry.Blob, err)
		}
		if err := s.checkObject(id, entry.Size); err != nil {
			return fmt.Errorf("backup: extras blob %s (%s): %w", entry.Blob, entry.Path, err)
		}
	}
	return nil
}

func (s *restoreState) restoreAuxiliary(
	ctx context.Context,
	m *Manifest,
) ([]RestoredAuxiliary, error) {
	restored := make([]RestoredAuxiliary, 0, len(m.Auxiliary))
	var total int64
	for _, artifact := range m.Auxiliary {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, err := pack.ParseBlobID(artifact.Blob)
		if err != nil {
			return nil, fmt.Errorf(
				"backup: auxiliary artifact %q blob id %q: %w",
				artifact.Name, artifact.Blob, err,
			)
		}
		stream, err := s.repo.OpenBlob(
			ctx, s.known, id, nil, s.app.PackFileExtension(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"backup: reading auxiliary artifact %q: %w",
				artifact.Name, err,
			)
		}
		data, size, err := readAuxiliaryArtifact(
			stream, artifact, maxAuxiliaryBytes-total,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"backup: reading auxiliary artifact %q: %w",
				artifact.Name, err,
			)
		}
		total += size
		restored = append(restored, RestoredAuxiliary{
			Name: artifact.Name, Format: artifact.Format,
			SHA256: artifact.SHA256, Data: data,
		})
	}
	return restored, nil
}

func readAuxiliaryArtifact(
	stream *BlobStream,
	artifact ManifestAuxiliary,
	remaining int64,
) (data []byte, size int64, resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	size = stream.Size()
	if size != artifact.Bytes {
		return nil, size, fmt.Errorf(
			"auxiliary artifact %q is %d bytes but manifest records %d",
			artifact.Name, size, artifact.Bytes,
		)
	}
	if size < 0 || remaining < 0 || size > remaining {
		return nil, size, fmt.Errorf(
			"auxiliary artifact %q has invalid aggregate size %d",
			artifact.Name, size,
		)
	}
	data = make([]byte, int(size))
	_, readErr := io.ReadFull(stream, data)
	if err := errors.Join(readErr, stream.Verify()); err != nil {
		return nil, size, err
	}
	if digest := pack.ComputeBlobID(data).String(); digest != artifact.SHA256 {
		return nil, size, fmt.Errorf(
			"auxiliary artifact %q digest differs from manifest",
			artifact.Name,
		)
	}
	return data, size, nil
}

// blobRuns is one page blob and every page-map run it backs.
type blobRuns struct {
	id   pack.BlobID
	runs []PageRun
}

// restoreDB materializes the database file into a staging temp inside the
// root and returns the temp's name: every page-map run is written at
// page*page_size, and every page is hash-verified against the page-hash map
// while its blob is still in memory. Work is grouped by pack, s.jobs packs
// in flight; the file writes are disjoint pwrite calls, safe concurrently.
// The temp is NOT renamed over dbRel here — publishRestoredDB does that
// only after the snapshot's attachment and extras content has also been
// read and verified and the database proof has passed, so any unreadable
// content blob or failed proof fails the restore while an Overwrite
// target's existing database is still intact. On error the temp is
// removed; on success the caller owns it.
func (s *restoreState) restoreDB(ctx context.Context, dbRel string, pm *PageMap, hm *PageHashMap) (string, error) {
	// A fresh, unpredictably named temp inside the root: a preexisting
	// symlink at dbRel is never opened, and the concurrent pwrite workers
	// below never touch dbRel itself.
	tmpRel := dbRel + ".restore-" + pack.NewPackID()
	f, err := s.root.OpenFile(tmpRel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("backup: creating restored database: %w", err)
	}
	materialized := false
	defer func() {
		_ = f.Close()
		if !materialized {
			_ = s.root.Remove(tmpRel)
		}
	}()
	size := int64(pm.PageCount * uint64(pm.PageSize))
	if err := f.Truncate(size); err != nil {
		return "", fmt.Errorf("backup: sizing restored database: %w", err)
	}

	runsByBlob := make(map[uint32][]PageRun)
	for _, r := range pm.Runs {
		runsByBlob[r.BlobIndex] = append(runsByBlob[r.BlobIndex], r)
	}
	groups := map[string][]blobRuns{}
	var order []string
	for i, id := range pm.Blobs {
		ie, ok := s.known[id]
		if !ok {
			return "", fmt.Errorf("backup: page blob %s not present in any index", id)
		}
		if _, seen := groups[ie.PackID]; !seen {
			order = append(order, ie.PackID)
		}
		groups[ie.PackID] = append(groups[ie.PackID], blobRuns{id: id, runs: runsByBlob[uint32(i)]})
	}

	s.done, s.doneByte = 0, 0
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreDB, Total: int64(pm.PageCount), BytesTotal: size,
	})
	err = s.runPackGroups(ctx, order, func(packID string) {
		s.restorePackPages(f, packID, groups[packID], pm.PageSize, hm)
	})
	if err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("backup: syncing restored database: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("backup: closing restored database: %w", err)
	}
	materialized = true
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreDB, Done: int64(pm.PageCount), Total: int64(pm.PageCount),
		BytesDone: size, BytesTotal: size, Final: true,
	})
	return tmpRel, nil
}

// restorePortableMetadata streams one verified logical metadata artifact into
// an application-built runtime database in Kit-owned private scratch, then
// copies the closed result through the held target root. The restorer never
// receives a path whose resolution depends on the caller-supplied target.
func (s *restoreState) restorePortableMetadata(
	ctx context.Context, dbRel string, metadata *ManifestMetadata, restorer MetadataRestorer,
) (tmpRel string, dbBytes int64, resultErr error) {
	var stagedRel string
	// Register this before every other cleanup defer so it runs last and sees
	// errors those defers add to resultErr. The caller installs ownership of a
	// successful temp only after this function returns nil.
	defer func() {
		if resultErr == nil || stagedRel == "" {
			return
		}
		if err := s.root.Remove(stagedRel); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr,
				fmt.Errorf("backup: removing failed portable database staging: %w", err))
		}
	}()
	if metadata == nil {
		return "", 0, errors.New("backup: portable metadata manifest is missing")
	}
	id, err := pack.ParseBlobID(metadata.Blob)
	if err != nil {
		return "", 0, fmt.Errorf("backup: portable metadata blob id %q: %w", metadata.Blob, err)
	}
	stream, err := openObject(ctx, s.repo, s.known, id, s.recipes, s.app.PackFileExtension())
	if err != nil {
		return "", 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	if stream.Size() != metadata.Bytes {
		return "", 0, fmt.Errorf(
			"backup: portable metadata blob is %d bytes but manifest records %d", stream.Size(), metadata.Bytes)
	}

	scratch, err := openRestoreScratch(s.root, s.repo.Path(stagingDirName))
	if err != nil {
		return "", 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, scratch.close()) }()
	privateRel, privateDir, err := scratch.mkdir("restore-metadata-")
	if err != nil {
		return "", 0, fmt.Errorf("backup: creating private metadata staging directory: %w", err)
	}
	defer func() {
		if err := scratch.root.RemoveAll(privateRel); err != nil {
			resultErr = errors.Join(resultErr,
				fmt.Errorf("backup: removing private metadata staging directory: %w", err))
		}
	}()
	privateDBRel := filepath.Join(privateRel, "runtime.db")
	privateDB := filepath.Join(privateDir, "runtime.db")
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Total: 1, BytesTotal: metadata.Bytes,
	})
	if err := restorer.RestoreMetadata(
		ctx, metadata.Format, stream, privateDB); err != nil {
		return "", 0, fmt.Errorf("backup: restoring portable metadata: %w", err)
	}
	if !stream.Verified() {
		return "", 0, errors.New("backup: metadata restorer returned before verified EOF")
	}
	info, err := scratch.root.Lstat(privateDBRel)
	if err != nil {
		return "", 0, fmt.Errorf("backup: checking restored metadata database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("backup: metadata restorer did not create a regular database file")
	}
	for _, sidecar := range sqliteSidecarNames(privateDBRel) {
		if _, err := scratch.root.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return "", 0, fmt.Errorf(
					"backup: metadata restorer left SQLite sidecar %s",
					filepath.Join(scratch.path, sidecar),
				)
			}
			return "", 0, fmt.Errorf("backup: checking metadata restore sidecar: %w", err)
		}
	}
	f, err := scratch.root.Open(privateDBRel)
	if err != nil {
		return "", 0, fmt.Errorf("backup: opening restored metadata database: %w", err)
	}
	openedInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", 0, fmt.Errorf("backup: inspecting restored metadata database: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		_ = f.Close()
		return "", 0, errors.New("backup: restored metadata database changed before confinement")
	}
	tmpRel, err = s.stageRootDatabase(ctx, dbRel, f, info.Size())
	closeErr := f.Close()
	stagedRel = tmpRel
	if err := errors.Join(err, closeErr); err != nil {
		return "", 0, fmt.Errorf("backup: confining restored metadata database: %w", err)
	}
	if err := s.verifyHeldTarget(tmpRel); err != nil {
		return "", 0, err
	}
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Done: 1, Total: 1,
		BytesDone: metadata.Bytes, BytesTotal: metadata.Bytes, Final: true,
	})
	return tmpRel, info.Size(), nil
}

// closePreparedRestoreDatabase is a narrow seam for exercising cleanup after
// a successful confinement whose final close reports an error.
var closePreparedRestoreDatabase = (*os.File).Close

// prepareBeforePublication gives the application an isolated copy of the
// unpublished database, then copies its closed replacement through the held
// target root. The callback must not be able to reach a target namespace that
// changed after Restore opened its root descriptor.
func (s *restoreState) prepareBeforePublication(
	ctx context.Context, currentRel, finalDBRel string,
	callback func(context.Context, RestorePublicationTarget) error,
) (replacementRel string, dbBytes int64, resultErr error) {
	var stagedRel string
	// Register first so it observes errors joined by scratch cleanup and close.
	// Once staging succeeds, every error path must remove the replacement.
	defer func() {
		if resultErr == nil || stagedRel == "" {
			return
		}
		if err := s.root.Remove(stagedRel); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr,
				fmt.Errorf("backup: removing failed prepared database staging: %w", err))
		}
	}()
	source, err := s.root.Open(currentRel)
	if err != nil {
		return "", 0, fmt.Errorf("backup: opening staged database for publication preparation: %w", err)
	}
	defer func() {
		if source != nil {
			resultErr = errors.Join(resultErr, source.Close())
		}
	}()
	sourceInfo, err := source.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("backup: inspecting staged database for publication preparation: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return "", 0, errors.New("backup: staged database for publication preparation is not a regular file")
	}

	scratch, err := openRestoreScratch(s.root, s.repo.Path(stagingDirName))
	if err != nil {
		return "", 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, scratch.close()) }()
	privateRel, _, err := scratch.mkdir("restore-publication-")
	if err != nil {
		return "", 0, fmt.Errorf("backup: creating private publication staging directory: %w", err)
	}
	defer func() {
		if err := scratch.root.RemoveAll(privateRel); err != nil {
			resultErr = errors.Join(resultErr,
				fmt.Errorf("backup: removing private publication staging directory: %w", err))
		}
	}()
	privateDBRel := filepath.Join(privateRel, filepath.Base(finalDBRel))
	privateDB := filepath.Join(scratch.path, privateDBRel)
	private, err := scratch.root.OpenFile(
		privateDBRel,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return "", 0, fmt.Errorf("backup: creating private publication database: %w", err)
	}
	defer func() {
		if private != nil {
			resultErr = errors.Join(resultErr, private.Close())
		}
	}()
	written, err := io.Copy(private, &contextio.Reader{Context: ctx, Reader: source})
	if err != nil {
		return "", 0, fmt.Errorf("backup: copying private publication database: %w", err)
	}
	if written != sourceInfo.Size() {
		return "", 0, fmt.Errorf(
			"backup: staged database is %d bytes but changed to %d bytes during publication preparation",
			sourceInfo.Size(), written)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if err := private.Sync(); err != nil {
		return "", 0, fmt.Errorf("backup: syncing private publication database: %w", err)
	}
	if err := private.Close(); err != nil {
		private = nil
		return "", 0, fmt.Errorf("backup: closing private publication database: %w", err)
	}
	private = nil
	if err := source.Close(); err != nil {
		source = nil
		return "", 0, fmt.Errorf("backup: closing staged database for publication preparation: %w", err)
	}
	source = nil

	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if err := callback(ctx, RestorePublicationTarget{TargetDir: s.target, DBPath: privateDB, LooseContent: s.looseContent}); err != nil {
		return "", 0, fmt.Errorf("backup: preparing restored application state: %w", err)
	}
	info, err := scratch.root.Lstat(privateDBRel)
	if err != nil {
		return "", 0, fmt.Errorf("backup: checking prepared restored database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("backup: publication callback did not leave a regular database file")
	}
	for _, sidecar := range sqliteSidecarNames(privateDBRel) {
		if _, err := scratch.root.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return "", 0, fmt.Errorf(
					"backup: publication callback left SQLite sidecar %s",
					filepath.Join(scratch.path, sidecar),
				)
			}
			return "", 0, fmt.Errorf("backup: checking publication callback sidecar: %w", err)
		}
	}
	opened, err := scratch.root.Open(privateDBRel)
	if err != nil {
		return "", 0, fmt.Errorf("backup: opening prepared restored database: %w", err)
	}
	openedInfo, err := opened.Stat()
	if err != nil {
		_ = opened.Close()
		return "", 0, fmt.Errorf("backup: inspecting prepared restored database: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		_ = opened.Close()
		return "", 0, errors.New("backup: prepared restored database changed before confinement")
	}
	var stageErr error
	stagedRel, stageErr = s.stageRootDatabase(ctx, finalDBRel, opened, info.Size())
	closeErr := closePreparedRestoreDatabase(opened)
	if err := errors.Join(stageErr, closeErr); err != nil {
		var cleanupErr error
		if stagedRel != "" {
			cleanupErr = s.root.Remove(stagedRel)
		}
		return "", 0, fmt.Errorf("backup: confining prepared restored database: %w", errors.Join(err, cleanupErr))
	}
	if err := s.root.Remove(currentRel); err != nil {
		removeErr := s.root.Remove(stagedRel)
		return "", 0, errors.Join(
			fmt.Errorf("backup: removing replaced staged database: %w", err),
			removeErr)
	}
	return stagedRel, info.Size(), nil
}

type restoreScratch struct {
	base     *os.Root
	root     *os.Root
	path     string
	relative string
}

// openRestoreScratch prefers the repository staging root when it is disjoint
// from the held restore target, then falls back to the system temporary root.
// Each candidate is resolved and pinned before use so callback paths never
// traverse the caller-controlled target namespace.
func openRestoreScratch(target *os.Root, repoStaging string) (*restoreScratch, error) {
	if target == nil {
		return nil, errors.New("backup: nil restore target root for publication scratch")
	}
	targetInfo, err := target.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("backup: inspecting restore target for publication scratch: %w", err)
	}
	var candidateErrs []error
	for _, candidate := range []string{repoStaging, os.TempDir()} {
		scratch, err := openRestoreScratchCandidate(targetInfo, candidate)
		if err == nil {
			return scratch, nil
		}
		candidateErrs = append(candidateErrs, err)
	}
	return nil, errors.Join(
		errors.New("backup: no private restore scratch outside target"),
		errors.Join(candidateErrs...),
	)
}

func openRestoreScratchCandidate(
	targetInfo fs.FileInfo,
	candidate string,
) (*restoreScratch, error) {
	scratchPath, err := filepath.Abs(candidate)
	if err != nil {
		return nil, fmt.Errorf("backup: resolving restore scratch %s: %w", candidate, err)
	}
	scratchPath, err = pathresolve.EvalSymlinks(scratchPath)
	if err != nil {
		return nil, fmt.Errorf("backup: resolving restore scratch %s: %w", candidate, err)
	}
	baseRoot, err := os.OpenRoot(scratchPath)
	if err != nil {
		return nil, fmt.Errorf("backup: opening restore scratch %s: %w", scratchPath, err)
	}
	closeOnError := func(err error) (*restoreScratch, error) {
		return nil, errors.Join(err, baseRoot.Close())
	}
	scratchInfo, err := baseRoot.Stat(".")
	if err != nil {
		return closeOnError(fmt.Errorf("backup: inspecting restore scratch %s: %w", scratchPath, err))
	}
	contained, err := pathContainsFilesystemIdentity(targetInfo, scratchInfo, scratchPath)
	if err != nil {
		return closeOnError(fmt.Errorf("backup: proving restore scratch %s: %w", scratchPath, err))
	}
	if contained {
		return closeOnError(fmt.Errorf(
			"backup: restore scratch %s is inside restore target", scratchPath))
	}
	var operationRel string
	created := false
	for range 100 {
		operationRel = "kit-restore-" + pack.NewPackID()
		if err := baseRoot.Mkdir(operationRel, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return closeOnError(fmt.Errorf("backup: creating private publication scratch: %w", err))
		}
		created = true
		break
	}
	if !created {
		return closeOnError(errors.New("backup: exhausted publication scratch name attempts"))
	}
	operationPath := filepath.Join(scratchPath, operationRel)
	cleanupOperation := func(operationRoot *os.Root, cause error) (*restoreScratch, error) {
		var closeErr error
		if operationRoot != nil {
			closeErr = operationRoot.Close()
		}
		return nil, errors.Join(cause, closeErr, baseRoot.RemoveAll(operationRel), baseRoot.Close())
	}
	if err := safefileio.EnsurePrivateDir(operationPath); err != nil {
		return cleanupOperation(nil,
			fmt.Errorf("backup: protecting private publication scratch: %w", err))
	}
	if err := safefileio.ValidatePrivateDir(operationPath); err != nil {
		return cleanupOperation(nil,
			fmt.Errorf("backup: validating private publication scratch: %w", err))
	}
	operationRoot, err := baseRoot.OpenRoot(operationRel)
	if err != nil {
		return cleanupOperation(nil,
			fmt.Errorf("backup: opening private publication scratch: %w", err))
	}
	heldOperation, err := operationRoot.Stat(".")
	if err != nil {
		return cleanupOperation(operationRoot,
			fmt.Errorf("backup: inspecting private publication scratch: %w", err))
	}
	pathOperation, err := os.Stat(operationPath)
	if err != nil || !os.SameFile(heldOperation, pathOperation) {
		return cleanupOperation(operationRoot, errors.Join(
			errors.New("backup: private publication scratch changed while opening it"), err))
	}
	return &restoreScratch{
		base: baseRoot, root: operationRoot, path: operationPath, relative: operationRel,
	}, nil
}

func pathContainsFilesystemIdentity(
	target fs.FileInfo,
	candidate fs.FileInfo,
	candidatePath string,
) (bool, error) {
	for current := candidatePath; ; current = filepath.Dir(current) {
		currentInfo, err := os.Stat(current)
		if err != nil {
			return false, err
		}
		if current == candidatePath && !os.SameFile(candidate, currentInfo) {
			return false, fmt.Errorf("scratch root %s was replaced while opening it", candidatePath)
		}
		if os.SameFile(target, currentInfo) {
			return true, nil
		}
		if filepath.Dir(current) == current {
			return false, nil
		}
	}
}

func (s *restoreScratch) mkdir(prefix string) (relative string, absolute string, err error) {
	for range 100 {
		name := prefix + pack.NewPackID()
		if err := s.root.Mkdir(name, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", "", err
		}
		return name, filepath.Join(s.path, name), nil
	}
	return "", "", errors.New("backup: exhausted private scratch name attempts")
}

func (s *restoreScratch) close() error {
	return errors.Join(s.root.Close(), s.base.Remove(s.relative), s.base.Close())
}

// publishRestoredDB swaps the fully materialized staging temp into place:
// stale SQLite sidecars are set aside, the temp is renamed over dbRel, and
// the asides are then removed. It runs only after every content blob the
// snapshot references — pages, attachments, extras — has been read and
// verified AND the staging database has passed its manifest-statistics check
// and optional integrity_check, so this is the
// single point where an Overwrite restore becomes destructive.
//
// The sidecar set-aside must precede the rename: a crash between the two
// steps must never leave the new database next to an old -wal/-shm pair or
// hot -journal that SQLite would replay over the proven bytes on its first
// normal open. Setting sidecars aside instead of removing them keeps a
// FAILED rename rollbackable — the asides are renamed back, so the target's
// live database keeps the WAL frames it needs — while a crashed publish
// leaves the aside bytes on disk for manual recovery (a later overwrite
// rerun sweeps them, removeRestoreTempFiles, because it replaces the
// database anyway). The final rename replaces a preexisting symlink at
// dbRel rather than following it.
func (s *restoreState) publishRestoredDB(tmpRel, dbRel string) error {
	asides, err := setAsideDBSidecars(s.root, dbRel)
	if err != nil {
		return errors.Join(err, putBackDBSidecars(s.root, asides))
	}
	if err := s.root.Rename(tmpRel, dbRel); err != nil {
		err = fmt.Errorf("backup: publishing restored database: %w", err)
		return errors.Join(err, putBackDBSidecars(s.root, asides))
	}
	for _, aside := range asides {
		if err := s.root.Remove(aside); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup: removing set-aside sidecar %s: %w", aside, err)
		}
	}
	return nil
}

// runPackGroups fans packIDs out to s.jobs workers, stopping dispatch on
// context cancellation or the first recorded failure, and returns the run's
// first error.
func (s *restoreState) runPackGroups(ctx context.Context, packIDs []string, work func(packID string)) error {
	packs := make(chan string)
	var wg sync.WaitGroup
	for range max(min(s.jobs, len(packIDs)), 1) {
		wg.Go(func() {
			for packID := range packs {
				if s.failed() {
					continue
				}
				work(packID)
			}
		})
	}
	for _, packID := range packIDs {
		if err := ctx.Err(); err != nil {
			s.fail(err)
			break
		}
		if s.failed() {
			break
		}
		packs <- packID
	}
	close(packs)
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr
}

// restorePackPages writes one pack's page blobs into the database file.
func (s *restoreState) restorePackPages(f *os.File, packID string, blobs []blobRuns, pageSize uint32, hm *PageHashMap) {
	pr, err := pack.OpenReader(s.repo.packPath(packID, s.app.PackFileExtension()), nil)
	if err != nil {
		s.fail(fmt.Errorf("backup: opening pack %s: %w", packID, err))
		return
	}
	defer func() { _ = pr.Close() }()
	entries := pr.Entries()
	entryByID := make(map[pack.BlobID]*pack.Entry, len(entries))
	for i := range entries {
		entryByID[entries[i].ID] = &entries[i]
	}
	for _, br := range blobs {
		if s.failed() {
			return
		}
		entry, ok := entryByID[br.id]
		if !ok {
			s.fail(fmt.Errorf("backup: page blob %s missing from pack %s footer", br.id, packID))
			return
		}
		raw, err := pr.ReadBlob(*entry)
		if err != nil {
			s.fail(fmt.Errorf("backup: reading page blob %s from pack %s: %w", br.id, packID, err))
			return
		}
		for _, run := range br.runs {
			if err := s.writeRun(f, raw, br.id, run, pageSize, hm); err != nil {
				s.fail(err)
				return
			}
		}
	}
}

// writeRun hash-verifies and writes one page-map run from its blob's bytes.
func (s *restoreState) writeRun(f *os.File, raw []byte, id pack.BlobID, run PageRun, pageSize uint32, hm *PageHashMap) error {
	length := uint64(run.PageCount) * uint64(pageSize)
	// Subtraction-based bounds check: BlobOffset is untrusted (decoded from
	// a page-map object), and BlobOffset+length can wrap uint64 for a huge
	// offset, slipping past an addition-based comparison into a slice panic.
	if run.BlobOffset > uint64(len(raw)) || length > uint64(len(raw))-run.BlobOffset {
		return fmt.Errorf(
			"backup: page map run (pages %d..%d) overruns blob %s (%d bytes at offset %d)",
			run.StartPage, run.StartPage+uint64(run.PageCount)-1, id, len(raw), run.BlobOffset)
	}
	segment := raw[run.BlobOffset : run.BlobOffset+length]
	for i := range uint64(run.PageCount) {
		p := run.StartPage + i
		h := PageHash(segment[i*uint64(pageSize) : (i+1)*uint64(pageSize)])
		if !bytes.Equal(h[:], hm.Hashes[p*pageHashSize:(p+1)*pageHashSize]) {
			return fmt.Errorf("backup: restored page %d does not match the snapshot's page hash map", p)
		}
	}
	if _, err := f.WriteAt(segment, int64(run.StartPage)*int64(pageSize)); err != nil {
		return fmt.Errorf("backup: writing pages %d..%d: %w", run.StartPage, run.StartPage+uint64(run.PageCount)-1, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done += int64(run.PageCount)
	s.doneByte += int64(length)
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreDB, Done: s.done, Total: int64(hm.PageCount),
		BytesDone: s.doneByte, BytesTotal: int64(hm.PageCount) * int64(pageSize),
	})
	return nil
}

// restoreAttachments lays the snapshot's attachment population out at the
// storage paths the restored database records for each hash (importers may
// namespace paths beyond the plain <hash[:2]>/<hash> layout), reading blobs
// grouped by pack. Every blob read re-derives its SHA-256 identity before
// any file is written.
func (s *restoreState) restoreAttachments(ctx context.Context, app App, m *Manifest, contentDir string) (int64, int64, error) {
	inventory, err := s.loadRestoreAttachmentInventory(ctx, app, m)
	if err != nil {
		return 0, 0, err
	}

	s.done, s.doneByte = 0, 0
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Total: int64(len(inventory.refs)), BytesTotal: m.Attachments.BlobBytes,
	})
	err = s.runPackGroups(ctx, inventory.order, func(packID string) {
		s.restorePackAttachments(ctx, contentDir, packID, inventory.groups[packID], inventory.paths, int64(len(inventory.refs)), m.Attachments.BlobBytes)
	})
	if err != nil {
		return 0, 0, err
	}
	if err := s.restoreChunkedAttachments(ctx, contentDir, inventory, m.Attachments.BlobBytes); err != nil {
		return 0, 0, err
	}
	var totalBytes int64
	for _, ref := range inventory.refs {
		totalBytes += ref.Size
	}
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Done: int64(len(inventory.refs)), Total: int64(len(inventory.refs)),
		BytesDone: totalBytes, BytesTotal: totalBytes, Final: true,
	})
	return int64(len(inventory.refs)), totalBytes, nil
}

// sqliteURIDSN builds a file: SQLite URI for path carrying rawQuery as its
// connection parameters. Built via url.URL so a path containing '?' or '#'
// cannot be misparsed as URI syntax — a naive path+"?params" concatenation
// would open a different (usually freshly created, empty) file when the
// path itself contains '?'. The path is made absolute first — a relative
// path must not become slash-rooted — then converted to the
// slash-separated, slash-rooted form SQLite's URI parser requires
// ("file:///C:/dir/app.db"): a raw drive-letter path would otherwise
// be read as a URI authority.
func sqliteURIDSN(path, rawQuery string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: rawQuery}).String()
}

// restoredDBDSN builds the immutable read-only SQLite URI for the restored
// database. immutable=1 matters: the file has no writers and no WAL sidecar,
// and an immutable open never creates -wal/-shm next to it.
func restoredDBDSN(dbPath string) string {
	return sqliteURIDSN(dbPath, "immutable=1&mode=ro")
}

// verifyHeldTarget re-proves that the caller-supplied target path still
// resolves to the directory the held root descriptor pins, and that the
// database file inside it is the same file the root sees. openRestoreRoot
// proved this once; SQLite opens re-resolve the path, so an actor who can
// rename the target in its parent could otherwise point them at a different
// database than the one restore wrote and hash-verified through the root.
func (s *restoreState) verifyHeldTarget(dbRel string) error {
	leaf, err := os.Lstat(s.target)
	if err != nil {
		return fmt.Errorf("backup: re-checking restore target: %w", err)
	}
	if leaf.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"backup: restore target %s was replaced with a symlink during restore", s.target)
	}
	held, err := s.root.Stat(".")
	if err != nil {
		return fmt.Errorf("backup: re-checking restore target: %w", err)
	}
	if !os.SameFile(leaf, held) {
		return fmt.Errorf(
			"backup: restore target %s was replaced during restore", s.target)
	}
	byPath, err := os.Stat(filepath.Join(s.target, dbRel))
	if err != nil {
		return fmt.Errorf("backup: re-checking restored database: %w", err)
	}
	byRoot, err := s.root.Stat(dbRel)
	if err != nil {
		return fmt.Errorf("backup: re-checking restored database: %w", err)
	}
	if !os.SameFile(byPath, byRoot) {
		return fmt.Errorf(
			"backup: restored database %s was replaced during restore",
			filepath.Join(s.target, dbRel))
	}
	return nil
}

// openRestoredDB opens the restored database read-only for the proof queries.
// SQLite can only open by path, which re-resolves the target directory, so
// after forcing the first connection open this re-verifies that the path
// still leads to the directory and database file the held root pins, and the
// callers verify once more after their queries complete. A replacement that
// persists at either check fails the restore; only a replace-and-restore
// race timed exactly around a connection's own open remains outside what a
// path-based open can detect.
func (s *restoreState) openRestoredDB(ctx context.Context) (*sql.DB, error) {
	dbRel := s.dbRead
	db, err := sqliteOpener(s.sqlite).OpenSQLite(filepath.Join(s.target, dbRel), SQLiteOpenOptions{
		Access: SQLiteReadOnlyImmutable,
	})
	if err != nil {
		return nil, fmt.Errorf("backup: opening restored database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("backup: opening restored database: %w", err)
	}
	if err := s.verifyHeldTarget(dbRel); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// restoredContentPaths opens the restored database read-only and asks the app
// to re-derive hash → relative content paths from it, so restore can
// materialize and verify every referenced file at the paths the app records.
func (s *restoreState) restoredContentPaths(ctx context.Context) (map[string][]string, error) {
	db, err := s.openRestoredDB(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	paths, err := s.app.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	// The derived paths decide where attachment bytes land inside the root;
	// fail closed if the database they came from is no longer the one the
	// root holds.
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return nil, err
	}
	return paths, nil
}

// restorePackAttachments writes one pack's attachment blobs to their
// recorded storage paths under dir.
func (s *restoreState) restorePackAttachments(
	ctx context.Context, contentDir, packID string, refs []ContentRef,
	paths map[string][]string, total, totalBytes int64,
) {
	pr, err := pack.OpenReader(s.repo.packPath(packID, s.app.PackFileExtension()), nil)
	if err != nil {
		s.fail(fmt.Errorf("backup: opening pack %s: %w", packID, err))
		return
	}
	defer func() { _ = pr.Close() }()
	entries := pr.Entries()
	entryByID := make(map[pack.BlobID]*pack.Entry, len(entries))
	for i := range entries {
		entryByID[entries[i].ID] = &entries[i]
	}
	for _, ref := range refs {
		if s.failed() {
			return
		}
		id, _ := pack.ParseBlobID(ref.Hash) // validated during grouping
		entry, ok := entryByID[id]
		if !ok {
			s.fail(fmt.Errorf("backup: attachment blob %s missing from pack %s footer", ref.Hash, packID))
			return
		}
		indexed := s.known[id]
		if entry.Offset != indexed.Offset || entry.StoredLen != indexed.StoredLen || entry.Flags != indexed.Flags {
			s.fail(fmt.Errorf("backup: attachment %s index entry disagrees with pack %s footer", ref.Hash, packID))
			return
		}
		if int64(entry.RawLen) != ref.Size {
			s.fail(fmt.Errorf(
				"backup: attachment %s is %d bytes but its list records %d", ref.Hash, entry.RawLen, ref.Size))
			return
		}
		// Every listed hash was proven to have at least one path before the
		// workers were dispatched (restoreAttachments' coverage check).
		for _, rel := range paths[ref.Hash] {
			// rel comes from the restored database via App.RestoredContentPaths,
			// so it is re-validated here before being joined into dir — the same
			// untrusted provenance stageExtrasEntry guards against for extras
			// tree paths.
			if !filepath.IsLocal(rel) {
				s.fail(fmt.Errorf(
					"backup: attachment %s restore path %q escapes the content directory", ref.Hash, rel))
				return
			}
			stream, err := pr.OpenBlob(ctx, *entry)
			if err != nil {
				s.fail(fmt.Errorf("backup: opening attachment %s from pack %s: %w", ref.Hash, packID, err))
				return
			}
			var writeErr error
			if s.compression.Enabled {
				writeErr = s.restoreCompressedContent(ctx, contentDir, ref, stream)
			} else {
				writeErr = s.writeRootReader(ctx, filepath.Join(contentDir, rel), stream, ref.Size, 0o600)
			}
			if err := errors.Join(writeErr, stream.Close()); err != nil {
				s.fail(err)
				return
			}
		}
		s.mu.Lock()
		s.done++
		s.doneByte += ref.Size
		s.progress.emit(ProgressEvent{
			Stage: ProgressStageAttachments, Done: s.done, Total: total,
			BytesDone: s.doneByte, BytesTotal: totalBytes,
		})
		s.mu.Unlock()
	}
}

// stagedFile records one extras file staged next to its final path: rel is
// where the file belongs, tmpRel the verified temp holding its bytes until
// promoteExtras renames it into place.
type stagedFile struct {
	rel    string
	tmpRel string
}

// stageExtras fetches, hash-verifies, and writes every extras file in the
// snapshot (the application-selected operational files, ExtrasSpec) to a
// temp sibling of its final path under the target, preserving recorded
// modes, and returns the staged set for promoteExtras. Nothing is renamed
// into place here: extras overwrite live operational files in an Overwrite
// target, so an unreadable blob — or any later failure up to the restore
// proof — must leave all of them untouched. On error the temps staged so
// far are removed.
func (s *restoreState) stageExtras(ctx context.Context, app App, m *Manifest) ([]stagedFile, error) {
	if m.Extras.Tree == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := pack.ParseBlobID(m.Extras.Tree)
	if err != nil {
		return nil, fmt.Errorf("backup: extras tree blob id %q: %w", m.Extras.Tree, err)
	}
	raw, err := s.fetch(id)
	if err != nil {
		return nil, err
	}
	var tree ExtrasTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("backup: extras tree %s: %w", id, err)
	}
	if err := checkExtrasCollisions(tree.Entries); err != nil {
		return nil, err
	}
	s.progress.emit(ProgressEvent{Stage: ProgressStageExtras, Total: int64(len(tree.Entries))})
	staged := make([]stagedFile, 0, len(tree.Entries))
	ok := false
	defer func() {
		if !ok {
			s.removeStagedFiles(staged)
		}
	}()
	for i, entry := range tree.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sf, err := s.stageExtrasEntry(ctx, app, entry)
		if err != nil {
			return nil, err
		}
		staged = append(staged, sf)
		s.progress.emit(ProgressEvent{
			Stage: ProgressStageExtras, Done: int64(i + 1), Total: int64(len(tree.Entries)),
		})
	}
	if len(staged) > 0 {
		s.progress.emit(ProgressEvent{
			Stage: ProgressStageExtras, Done: int64(len(staged)), Total: int64(len(staged)),
			Final: true,
		})
	}
	ok = true
	return staged, nil
}

// promoteExtras renames every staged extras temp over its final path. It
// runs after the restore proof and immediately before publishRestoredDB, so
// the window in which an Overwrite target can hold new extras next to its
// old database is a short run of renames that no blob read or proof query
// can widen. A partial failure leaves the already-promoted files in place —
// their bytes were verified at staging time — and the caller removes the
// remaining temps.
func (s *restoreState) promoteExtras(staged []stagedFile) error {
	for _, sf := range staged {
		if err := s.promoteRootFile(sf.tmpRel, sf.rel); err != nil {
			return err
		}
	}
	return nil
}

// removeStagedFiles removes staged temps after a failed restore. Temps a
// promotion already renamed away are simply gone; removal is best-effort
// because it runs on paths this restore itself created.
func (s *restoreState) removeStagedFiles(staged []stagedFile) {
	for _, sf := range staged {
		_ = s.root.Remove(sf.tmpRel)
	}
}

// foldedPathKey is the collision key restore uses to detect distinct archive
// paths that alias one file on the restoring host: case-insensitive
// filesystems (default macOS, Windows) fold "A/b" and "a/b" onto one file,
// and normalization-insensitive ones (APFS, HFS+) additionally resolve the
// NFC and NFD spellings of a name to one file — so the key Unicode-normalizes
// the cleaned OS form of the path before case-folding it. Both the attachment
// and extras collision checks must use this same key.
func foldedPathKey(rel string) string {
	return strings.ToLower(norm.NFC.String(filepath.Clean(filepath.FromSlash(rel))))
}

// checkExtrasCollisions rejects an extras tree in which two entries resolve
// to one file. writeRootFile publishes each entry with a clobbering rename,
// so a duplicate path — or two paths differing only by case, which alias one
// file on case-insensitive filesystems — would silently keep whichever entry
// restored last while restore still reported every entry as written. As with
// the attachment check, this deliberately also rejects such trees on
// case-sensitive hosts: a snapshot that only restores correctly on a
// case-sensitive filesystem is not safe to call restorable.
func checkExtrasCollisions(entries []ExtrasEntry) error {
	owner := make(map[string]string, len(entries))
	for _, entry := range entries {
		key := foldedPathKey(entry.Path)
		other, ok := owner[key]
		switch {
		case !ok:
			owner[key] = entry.Path
		case other == entry.Path:
			return fmt.Errorf("backup: extras tree lists path %q twice", entry.Path)
		default:
			return fmt.Errorf(
				"backup: extras entries %q and %q collide under case-folded key %q",
				other, entry.Path, key)
		}
	}
	return nil
}

// trailingDotOrSpaceComponent returns the first component of rel (a relative
// path in slash or OS-separator form) that ends in a dot or space, or "" when
// none does. Windows resolves such a component to its trimmed name, so two
// distinct-looking archive paths can alias the same file; restore rejects them
// on every platform to keep its reserved-name and path-collision guards sound
// regardless of the restoring host's OS.
func trailingDotOrSpaceComponent(rel string) string {
	for comp := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if strings.TrimRight(comp, ". ") != comp {
			return comp
		}
	}
	return ""
}

// validateExtrasEntryPath checks one extras entry path against the rules
// restore enforces before writing, returning the cleaned OS-form path the
// entry restores to. Entry paths come from a decoded tree blob, so they are
// re-validated: only local, relative, traversal-free paths may be written
// under the target, and never paths that overlap the database or attachments
// restore already produced. Verify runs the same checks so a snapshot that
// restore would refuse cannot verify cleanly.
//
// The path is cleaned before validating: the final filepath.Join cleans it
// anyway, so validating the raw form would let "safe/../app.db" pass the
// reserved-name check yet still land on a reserved path. Components ending
// in a dot or space resolve to the trimmed name on Windows, so "content."
// would alias the reserved content dir past the folded comparison; they are
// rejected on every platform to keep the guard sound regardless of OS. The
// reserved names come from the app so a generic application's extras can
// never overwrite its restored DB or content tree; the comparison is folded
// because the default macOS filesystem is case-insensitive.
func validateExtrasEntryPath(path, contentDirName, dbFileName string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(path))
	if path == "" || rel == "." || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("backup: extras entry path %q escapes the restore target", path)
	}
	if trailingDotOrSpaceComponent(rel) != "" {
		return "", fmt.Errorf("backup: extras entry path %q has a component ending in a dot or space", path)
	}
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	for _, reserved := range append(
		[]string{contentDirName, dbFileName}, sqliteSidecarNames(dbFileName)...) {
		if strings.EqualFold(first, reserved) {
			return "", fmt.Errorf("backup: extras entry path %q overlaps restored archive content", path)
		}
	}
	return rel, nil
}

// stageExtrasEntry validates one extras entry and stages its verified bytes
// next to the final path. Capture never records archive content as an
// extra, so an entry naming the restored database, its SQLite sidecars, or
// the content tree can only come from a tampered tree blob trying to
// overwrite already-proven outputs (validateExtrasEntryPath).
func (s *restoreState) stageExtrasEntry(ctx context.Context, app App, entry ExtrasEntry) (stagedFile, error) {
	rel, err := validateExtrasEntryPath(entry.Path, app.ContentDirName(), app.DBFileName())
	if err != nil {
		return stagedFile{}, err
	}
	id, err := pack.ParseBlobID(entry.Blob)
	if err != nil {
		return stagedFile{}, fmt.Errorf(
			"backup: extras entry %s blob id %q: %w", entry.Path, entry.Blob, err)
	}
	stream, err := openObject(ctx, s.repo, s.known, id, s.recipes, s.app.PackFileExtension())
	if err != nil {
		return stagedFile{}, err
	}
	mode := os.FileMode(entry.Mode).Perm()
	if mode == 0 {
		mode = 0o600
	}
	tmpRel, stageErr := s.stageRootReaderWithOptions(ctx, rel, stream, entry.Size, mode, ".restore-", uint64(MaxObjectBytes))
	if err := errors.Join(stageErr, stream.Close()); err != nil {
		if tmpRel != "" {
			_ = s.root.Remove(tmpRel)
		}
		return stagedFile{}, fmt.Errorf("backup: restoring extras entry %s: %w", entry.Path, err)
	}
	return stagedFile{rel: rel, tmpRel: tmpRel}, nil
}

// enterRestoreDir opens one directory component beneath cur as its own root,
// creating it if missing and refusing symlinks: os.Root only refuses symlinks
// that escape the root, so without this check a preexisting in-root symlink
// (overwrite mode merges into an existing tree) would silently redirect
// restored writes elsewhere inside the target — e.g. "deletions -> content"
// would land extras files in the already-proven content tree. The component
// must lstat as a real directory and the opened descriptor must be that same
// inode, so a symlink present up front and one raced in before the open both
// fail closed. Mirrors the capture-side enterExtrasDir.
func enterRestoreDir(cur *os.Root, comp string) (*os.Root, error) {
	li, err := cur.Lstat(comp)
	if errors.Is(err, os.ErrNotExist) {
		if mkErr := cur.Mkdir(comp, 0o700); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
			return nil, mkErr
		}
		li, err = cur.Lstat(comp)
	}
	if err != nil {
		return nil, err
	}
	if li.Mode()&os.ModeSymlink != 0 || !li.IsDir() {
		return nil, fmt.Errorf("path component %q is not a real directory", comp)
	}
	sub, err := cur.OpenRoot(comp)
	if err != nil {
		return nil, err
	}
	held, err := sub.Stat(".")
	if err != nil {
		_ = sub.Close()
		return nil, err
	}
	if !os.SameFile(li, held) {
		_ = sub.Close()
		return nil, fmt.Errorf("path component %q changed during restore", comp)
	}
	return sub, nil
}

// openLeafDir walks rel's parent components beneath the confined root,
// creating and verifying each one (enterRestoreDir — no symlink is ever
// followed, even one resolving inside the root), and returns the held
// directory root plus rel's leaf name. The caller must release the returned
// root with closeLeafDir.
func (s *restoreState) openLeafDir(rel string) (*os.Root, string, error) {
	dir := s.root
	comps := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
	for _, comp := range comps[:len(comps)-1] {
		sub, err := enterRestoreDir(dir, comp)
		s.closeLeafDir(dir)
		if err != nil {
			return nil, "", fmt.Errorf("backup: creating restore directory for %s: %w", rel, err)
		}
		dir = sub
	}
	return dir, comps[len(comps)-1], nil
}

// closeLeafDir releases a root returned by openLeafDir; the target root
// itself (a single-component rel) is left open for the rest of the restore.
func (s *restoreState) closeLeafDir(dir *os.Root) {
	if dir != s.root {
		_ = dir.Close()
	}
}

// writeRootReader streams exactly expected bytes into private staging and
// publishes the file only after the source reaches its verified terminal EOF.
func (s *restoreState) writeRootReader(
	ctx context.Context, rel string, src io.Reader, expected int64, mode os.FileMode,
) error {
	tmpRel, err := s.stageRootReader(ctx, rel, src, expected, mode)
	if err != nil {
		return err
	}
	if err := s.promoteRootFile(tmpRel, rel); err != nil {
		_ = s.root.Remove(tmpRel)
		return err
	}
	return nil
}

// stageRootReader writes to a fresh, unpredictably named temp entered through
// verified directory components. Reading one byte beyond the recorded size
// distinguishes a forged short size from verified EOF without allowing an
// unbounded source to fill the target. The temp is synced before return.
func (s *restoreState) stageRootReader(
	ctx context.Context, rel string, src io.Reader, expected int64, mode os.FileMode,
) (string, error) {
	return s.stageRootReaderWithOptions(ctx, rel, src, expected, mode, ".restore-", pack.MaxRawLen)
}

// stageRootDatabase confines an application-built runtime database without
// imposing the pack format's per-blob ceiling. Its temp name intentionally
// matches removeRestoreTempFiles' crash-recovery sweep.
func (s *restoreState) stageRootDatabase(
	ctx context.Context, rel string, src io.Reader, expected int64,
) (string, error) {
	return s.stageRootReaderWithOptions(
		ctx, rel, src, expected, 0o600, filepath.Base(rel)+".restore-", 0)
}

func (s *restoreState) stageRootReaderWithOptions(
	ctx context.Context, rel string, src io.Reader, expected int64, mode os.FileMode,
	tempPrefix string, maxBytes uint64,
) (string, error) {
	if ctx == nil {
		return "", errors.New("backup: nil restore context")
	}
	if expected < 0 || (maxBytes > 0 && uint64(expected) > maxBytes) {
		return "", fmt.Errorf("backup: restored file %s has invalid recorded size %d", rel, expected)
	}
	dir, _, err := s.openLeafDir(rel)
	if err != nil {
		return "", err
	}
	defer s.closeLeafDir(dir)
	tmp := tempPrefix + pack.NewPackID()
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", fmt.Errorf("backup: creating restored file %s: %w", rel, err)
	}
	staged := false
	defer func() {
		if !staged {
			_ = dir.Remove(tmp)
		}
	}()
	// O_CREATE's mode is filtered through the umask; restore must reproduce
	// the captured mode exactly.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("backup: setting mode on restored file %s: %w", rel, err)
	}
	reader := &contextio.Reader{Context: ctx, Reader: src}
	written, copyErr := io.CopyBuffer(f, io.LimitReader(reader, expected), make([]byte, 64<<10))
	if copyErr != nil {
		_ = f.Close()
		return "", fmt.Errorf("backup: writing restored file %s: %w", rel, copyErr)
	}
	if written != expected {
		_ = f.Close()
		return "", fmt.Errorf("backup: restored file %s is %d bytes but its record says %d", rel, written, expected)
	}
	var extra [1]byte
	n, readErr := reader.Read(extra[:])
	if n != 0 || !errors.Is(readErr, io.EOF) {
		_ = f.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", fmt.Errorf("backup: checking restored file %s terminal EOF: %w", rel, readErr)
		}
		return "", fmt.Errorf("backup: restored file %s has more bytes than its record says %d", rel, expected)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("backup: syncing restored file %s: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("backup: closing restored file %s: %w", rel, err)
	}
	staged = true
	return filepath.Join(filepath.Dir(filepath.Clean(filepath.FromSlash(rel))), tmp), nil
}

// promoteRootFile renames a staged temp over its final leaf name. The leaf
// directory is re-entered component by component (enterRestoreDir), so a
// symlink raced into the path between staging and promotion still fails
// closed, and the rename happens within the held directory descriptor.
func (s *restoreState) promoteRootFile(tmpRel, rel string) error {
	dir, name, err := s.openLeafDir(rel)
	if err != nil {
		return err
	}
	defer s.closeLeafDir(dir)
	if err := dir.Rename(filepath.Base(tmpRel), name); err != nil {
		return fmt.Errorf("backup: publishing restored file %s: %w", rel, err)
	}
	return nil
}

// proveRestoredDB compares restored application statistics with the capture
// manifest and optionally runs SQLite's full integrity_check first. Page-level
// identity was already proven against the page-hash map during materialization.
// The checks read the staging temp before publishRestoredDB, so failure leaves
// an Overwrite target's existing database untouched. checked is called only
// after an enabled integrity_check passes.
func (s *restoreState) proveRestoredDB(
	ctx context.Context, m *Manifest, checkIntegrity bool, checked func(),
) error {
	db, err := s.openRestoredDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if checkIntegrity {
		if err := restoreIntegrityCheck(ctx, db); err != nil {
			return err
		}
		if checked != nil {
			checked()
		}
	}

	restoredStats, err := s.app.RestoredStats(ctx, db)
	if err != nil {
		return err
	}
	// The proof is only about the database the root holds; fail closed if the
	// path the queries ran against no longer resolves to it.
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	if !bytes.Equal(compactJSON(restoredStats), compactJSON(m.Stats)) {
		return fmt.Errorf("backup: restored database stats %s do not match manifest stats %s",
			restoredStats, m.Stats)
	}
	return nil
}

func restoreIntegrityCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("backup: restored database integrity_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var findings []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("backup: reading integrity_check result: %w", err)
		}
		findings = append(findings, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: integrity_check rows: %w", err)
	}
	if len(findings) != 1 || findings[0] != "ok" {
		return fmt.Errorf("backup: restored database failed integrity_check: %v", findings)
	}
	return nil
}

// compactJSON normalizes RawMessage formatting: manifests on disk are
// indented, and a RawMessage captured from an indented document keeps that
// indentation, while freshly marshaled stats are compact.
func compactJSON(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}
