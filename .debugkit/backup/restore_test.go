package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/safefileio"
)

type restoreTargetCoordinatorFunc func(
	context.Context, *os.Root,
) (RestoreTargetLease, error)

func (f restoreTargetCoordinatorFunc) AcquireRestoreTarget(
	ctx context.Context, root *os.Root,
) (RestoreTargetLease, error) {
	return f(ctx, root)
}

type restoreTargetLeaseFunc func() error

func (f restoreTargetLeaseFunc) Release() error { return f() }

func writeLargeAttachment(t *testing.T, dir string, size int64, compressible bool) ContentRef {
	t.Helper()
	require := require.New(t)
	require.Positive(size)
	tmp, err := os.CreateTemp(t.TempDir(), "large-attachment-*")
	require.NoError(err)
	digest := sha256.New()
	chunk := make([]byte, 96<<10)
	if compressible {
		copy(chunk, bytes.Repeat([]byte("streaming backup content "), len(chunk)/25+1))
	}
	var state uint32 = 1
	dst := io.MultiWriter(tmp, digest)
	remaining := size
	for remaining > 0 {
		if !compressible {
			for i := range chunk {
				state ^= state << 13
				state ^= state >> 17
				state ^= state << 5
				chunk[i] = byte(state)
			}
		}
		n := min(int64(len(chunk)), remaining)
		_, err = dst.Write(chunk[:n])
		require.NoError(err)
		remaining -= n
	}
	require.NoError(tmp.Close())
	hash := hex.EncodeToString(digest.Sum(nil))
	rel := filepath.Join(hash[:2], hash)
	require.NoError(os.MkdirAll(filepath.Join(dir, hash[:2]), 0o700))
	require.NoError(os.Rename(tmp.Name(), filepath.Join(dir, rel)))
	return ContentRef{Hash: hash, Size: size, StoragePath: filepath.ToSlash(rel)}
}

func hashFileStream(t *testing.T, path string) (int64, string) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	digest := sha256.New()
	size, copyErr := io.CopyBuffer(digest, f, make([]byte, 64<<10))
	require.NoError(t, errors.Join(copyErr, f.Close()))
	return size, hex.EncodeToString(digest.Sum(nil))
}

// TestSyncRestoredTreeCoversCreatedAncestors pins the durability pass for a
// restore target whose ancestors did not exist before the restore: the sync
// must cover the restored tree deepest-first, then climb through the created
// ancestors to the pre-existing ceiling that received the topmost new entry.
// Not parallel: it stubs the package-level pack.SyncDir hook.
func TestSyncRestoredTreeCoversCreatedAncestors(t *testing.T) {
	require := require.New(t)
	base := t.TempDir()
	target := filepath.Join(base, "a", "b", "out")

	ceiling := restoreSyncCeiling(target)
	require.Equal(base, ceiling, "the ceiling is the deepest ancestor existing before creation")

	require.NoError(os.MkdirAll(filepath.Join(target, "content", "aa"), 0o700))
	require.Equal(target, restoreSyncCeiling(target),
		"a target that already exists is its own ceiling; nothing above it gains entries")

	var synced []string
	origSyncDir := pack.SyncDir
	pack.SyncDir = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { pack.SyncDir = origSyncDir })

	require.NoError(syncRestoredTree(target, ceiling))
	require.Equal([]string{
		filepath.Join(target, "content", "aa"),
		filepath.Join(target, "content"),
		target,
		filepath.Join(base, "a", "b"),
		filepath.Join(base, "a"),
		base,
	}, synced, "deepest first: every directory's entry is durable in its parent before that parent syncs")

	synced = nil
	require.NoError(syncRestoredTree(target, target))
	require.Equal(target, synced[len(synced)-1],
		"with a pre-existing target the sync stops at the target itself")
}

func fileSHA256(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return sha256.Sum256(data)
}

func restorePublicationScratchDirs(t *testing.T, r *Repo) []string {
	t.Helper()
	entries, err := os.ReadDir(r.Path(stagingDirName))
	require.NoError(t, err)
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "restore-publication-") {
			dirs = append(dirs, entry.Name())
		}
	}
	return dirs
}

func restoreDatabaseStageFiles(t *testing.T, target, dbFileName string) []string {
	t.Helper()
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	var files []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), dbFileName+".restore-") {
			files = append(files, entry.Name())
		}
	}
	return files
}

// snapshotDirHashes maps every regular file under root (relative path) to
// its content hash, for whole-tree equality comparisons.
func snapshotDirHashes(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		out[rel] = fileSHA256(t, path)
		return nil
	})
	require.NoError(t, err)
	return out
}

// TestRestoreReproducesArchiveByteForByte is the restore proof's proof: a
// restored snapshot's database is byte-identical to the live database file
// as it existed at capture time, attachments and extras land byte-identical
// in the live layout, and this holds for a parent snapshot restored from an
// incremental chain, not just the latest.
func TestRestoreReproducesArchiveByteForByte(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, writer := seedBackupFixture(t)
	cacheDir := t.TempDir()

	// An extras file rides along (the deletions dir is always captured).
	deletionsPath := filepath.Join(dataDir, "deletions", "manifest-1.json")
	require.NoError(os.MkdirAll(filepath.Dir(deletionsPath), 0o700))
	require.NoError(os.WriteFile(deletionsPath, []byte(`{"id":"manifest-1"}`), 0o640))
	// WriteFile's mode is umask-filtered; pin the mode so the
	// capture-and-restore round trip has a distinctive value to preserve.
	require.NoError(os.Chmod(deletionsPath, 0o640))

	m1, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	// Create checkpoint-truncated the WAL inside the freeze and nothing has
	// written since, so the on-disk file is exactly the captured state.
	dbAtSnap1, err := os.ReadFile(dbPath)
	require.NoError(err)

	// Mutate the archive and take an incremental child snapshot. One added
	// attachment lives in the loose layout, one under an importer-style
	// namespaced storage path — restore must reproduce both placements. The
	// namespace deliberately starts with "http": only http:// and https://
	// URLs are excluded from capture, never local paths sharing the prefix.
	_, err = writer.ExecContext(ctx, `INSERT INTO notes (created_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)
	newRef := writeLooseAttachment(t, attachmentsDir, []byte("attachment added after snapshot 1"))
	nsRef := writeNamespacedAttachment(t, attachmentsDir, "http-cache", []byte("namespaced attachment"))
	_, err = writer.ExecContext(ctx,
		`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
		 VALUES (?, ?, ?, '', ''), (?, ?, ?, '', '')`,
		newRef.Hash, newRef.Hash[:2]+"/"+newRef.Hash, newRef.Size,
		nsRef.Hash, nsRef.StoragePath, nsRef.Size)
	require.NoError(err)
	m2, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Equal(m1.SnapshotID, m2.ParentID)
	dbAtSnap2, err := os.ReadFile(dbPath)
	require.NoError(err)

	// Restore the latest snapshot and compare byte-for-byte.
	target2 := filepath.Join(t.TempDir(), "restore-2")
	var events []ProgressEvent
	res2, err := Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target2,
		Progress:  func(ev ProgressEvent) { events = append(events, ev) },
	})
	require.NoError(err)
	assert.Equal(m2.SnapshotID, res2.SnapshotID)
	assert.True(res2.DatabaseIntegrityChecked)
	restored2, err := os.ReadFile(res2.DBPath)
	require.NoError(err)
	require.True(bytes.Equal(dbAtSnap2, restored2),
		"restored database must be byte-identical to the live file at capture time")
	assert.Equal(m2.Attachments.Blobs, res2.AttachmentBlobs)
	assert.Equal(m2.Attachments.BlobBytes, res2.AttachmentBytes)

	// Attachments land in the loose layout with matching bytes.
	assert.Equal(snapshotDirHashes(t, attachmentsDir), snapshotDirHashes(t, filepath.Join(target2, "content")))

	// Extras land at their captured relative path with mode preserved.
	restoredDeletions := filepath.Join(target2, "deletions", "manifest-1.json")
	assert.Equal(fileSHA256(t, deletionsPath), fileSHA256(t, restoredDeletions))
	if runtime.GOOS != "windows" {
		// Windows has no POSIX permission bits — Stat reports 0666 for any
		// writable file — so the exact-mode round trip is POSIX-only.
		info, err := os.Stat(restoredDeletions)
		require.NoError(err)
		assert.Equal(os.FileMode(0o640), info.Mode().Perm())
	}

	// Restoring the PARENT from the incremental chain reproduces the older
	// state, not the current one.
	target1 := filepath.Join(t.TempDir(), "restore-1")
	res1, err := Restore(ctx, r, newTestApp(), RestoreOptions{SnapshotID: m1.SnapshotID, TargetDir: target1})
	require.NoError(err)
	restored1, err := os.ReadFile(res1.DBPath)
	require.NoError(err)
	require.True(bytes.Equal(dbAtSnap1, restored1),
		"restoring the parent snapshot must reproduce the pre-mutation database")
	_, err = os.Stat(filepath.Join(target1, "content", newRef.Hash[:2], newRef.Hash))
	require.ErrorIs(err, os.ErrNotExist,
		"an attachment added after snapshot 1 must not appear in snapshot 1's restore")

	// Progress: each materialization and database-check stage completed.
	final := map[ProgressStage]ProgressEvent{}
	for _, ev := range events {
		if ev.Final {
			final[ev.Stage] = ev
		}
	}
	for _, stage := range []ProgressStage{
		ProgressStageRestoreDB,
		ProgressStageAttachments,
		ProgressStageExtras,
		ProgressStageIntegrityCheck,
		ProgressStageRestoreStats,
	} {
		require.Contains(final, stage)
		assert.Equal(final[stage].Done, final[stage].Total, "stage %s must finish complete", stage)
	}
	extrasFinalIndex := slices.IndexFunc(events, func(event ProgressEvent) bool {
		return event.Stage == ProgressStageExtras && event.Final
	})
	integrityStartIndex := slices.IndexFunc(events, func(event ProgressEvent) bool {
		return event.Stage == ProgressStageIntegrityCheck
	})
	require.NotEqual(-1, extrasFinalIndex)
	require.NotEqual(-1, integrityStartIndex)
	assert.Less(extrasFinalIndex, integrityStartIndex,
		"extras must report completion before database checks begin")
}

// collidingContentPathApp maps every content hash to one shared restore path,
// modeling an App whose derivation returns distinct blobs at the same relative
// path.
type collidingContentPathApp struct{ App }

func (a collidingContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(paths))
	for hash := range paths {
		out[hash] = []string{"shared/collision"}
	}
	return out, nil
}

// TestRestoreRejectsCollidingAttachmentPaths pins the path-collision guard: two
// different content hashes claiming the same restore path must fail the restore
// rather than have the parallel writer's temp-then-rename clobber one blob with
// the other and still report success.
func TestRestoreRejectsCollidingAttachmentPaths(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	_, err = Restore(ctx, r, collidingContentPathApp{App: newTestApp()}, RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "restore"),
	})
	require.ErrorContains(err, "two different attachments")
	require.ErrorContains(err, "shared/collision")
}

// caseFoldingContentPathApp assigns two distinct content hashes to restore
// paths that differ only in case ("A/b" and "a/b"). The assignment is keyed on
// sorted hash order so it is stable across the randomized map iteration.
type caseFoldingContentPathApp struct{ App }

func (a caseFoldingContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(paths))
	for hash := range paths {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	rels := []string{"A/b", "a/b"}
	out := make(map[string][]string, len(paths))
	for i, hash := range hashes {
		out[hash] = []string{rels[i%len(rels)]}
	}
	return out, nil
}

// TestRestoreRejectsCaseFoldingAttachmentPaths pins that two distinct blobs
// whose restore paths differ only in case collide on every platform: a
// case-insensitive filesystem would clobber one with the other, so restore
// rejects the pair up front rather than reporting a lossy success.
func TestRestoreRejectsCaseFoldingAttachmentPaths(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	_, err = Restore(ctx, r, caseFoldingContentPathApp{App: newTestApp()}, RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "restore"),
	})
	require.ErrorContains(err, "case-folded key")
	require.ErrorContains(err, "two different attachments")
}

// badContentPathApp maps every content hash to one fixed restore path, for
// exercising restore's validation of paths the restored database records.
type badContentPathApp struct {
	App
	path string
}

func (a badContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(paths))
	for hash := range paths {
		out[hash] = []string{a.path}
	}
	return out, nil
}

// TestLargePlainAttachmentCaptureVerifyRestore crosses the former 64 MiB
// whole-content ceiling without constructing the object in memory. The second
// snapshot exercises the incremental known-blob path, which must still stream
// and verify live content before reusing repository authority.
func TestLargePlainAttachmentCaptureVerifyRestore(t *testing.T) {
	largeBytes := largeBackupStreamTestBytes(t, 64<<20+1)
	for _, tc := range []struct {
		name         string
		compressible bool
		wantFlag     bool
	}{
		{name: "compressed", compressible: true, wantFlag: true},
		{name: "raw", compressible: false, wantFlag: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			ctx := t.Context()
			r := initTestRepo(t)
			dbPath, attachmentsDir, dataDir, writer := seedBackupFixture(t)
			large := writeLargeAttachment(t, attachmentsDir, largeBytes, tc.compressible)
			_, err := writer.ExecContext(ctx,
				`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
				 VALUES (?, ?, ?, '', '')`, large.Hash, large.StoragePath, large.Size)
			require.NoError(err)

			opts := createOpts(dbPath, attachmentsDir, dataDir, t.TempDir())
			first, err := Create(ctx, r, newTestApp(), opts)
			require.NoError(err)
			id, err := pack.ParseBlobID(large.Hash)
			require.NoError(err)
			known, err := r.LoadBlobIndex()
			require.NoError(err)
			recipes, err := loadObjectRecipes(ctx, r, known, first, testPackExt)
			require.NoError(err)
			recipe, chunked := recipes[id]
			require.True(chunked, "large attachments must use chunk recipes")
			id, err = pack.ParseBlobID(recipe.Chunks[0].Blob)
			require.NoError(err)
			require.Equal(tc.wantFlag, known[id].Flags&pack.BlobCompressed != 0)

			second, err := Create(ctx, r, newTestApp(), opts)
			require.NoError(err)
			require.Equal(first.SnapshotID, second.ParentID)
			require.Less(second.BytesAdded, large.Size,
				"incremental capture must reuse the verified large content blob")

			sawPartialObjectProgress := false
			verified, err := Verify(ctx, r, newTestApp(), VerifyOptions{
				SnapshotID: second.SnapshotID, Jobs: 2,
				Progress: func(event ProgressEvent) {
					if event.BytesDone > 0 && event.BytesDone < large.Size {
						sawPartialObjectProgress = true
					}
				},
			})
			require.NoError(err)
			require.Empty(verified.Problems)
			require.True(sawPartialObjectProgress, "verification must report bytes before a large object finishes")

			target := filepath.Join(t.TempDir(), "large-restore")
			_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
				SnapshotID: second.SnapshotID, TargetDir: target, Jobs: 2,
			})
			require.NoError(err)
			size, hash := hashFileStream(t, filepath.Join(target, "content", filepath.FromSlash(large.StoragePath)))
			require.Equal(large.Size, size)
			require.Equal(large.Hash, hash)
		})
	}
}

func largeBackupStreamTestBytes(t *testing.T, fallback int64) int64 {
	t.Helper()
	value := os.Getenv("KIT_STREAM_TEST_BYTES")
	if value == "" {
		return fallback
	}
	size, err := strconv.ParseInt(value, 10, 64)
	require.NoError(t, err)
	require.Positive(t, size)
	return size
}

// TestRestoreRejectsBadAttachmentPaths pins that restore refuses attachment
// paths a tampered or buggy database could record: a component ending in a
// dot or space (which Windows trims, aliasing a different name), and any "."
// or ".." component — including paths like "." or "safe/.." that pass
// filepath.IsLocal yet clean to the content directory itself, which restore
// must never write as a file. All are rejected on every platform before any
// write.
func TestRestoreRejectsBadAttachmentPaths(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	for _, path := range []string{"blobs./file", ".", "safe/..", "safe/../x"} {
		_, err = Restore(ctx, r, badContentPathApp{App: newTestApp(), path: path}, RestoreOptions{
			TargetDir: filepath.Join(t.TempDir(), "restore"),
		})
		require.ErrorContains(err, "ending in a dot or space", "path %q", path)
	}
}

// TestRestoreRefusesSymlinkTarget pins that a restore target whose final
// component is a symlink is refused: os.ReadDir and os.OpenRoot both follow it,
// so restore would otherwise materialize the archive under the link's target.
func TestRestoreRefusesSymlinkTarget(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "target-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: link})
	require.ErrorContains(err, "is a symlink")

	entries, err := os.ReadDir(realDir)
	require.NoError(err)
	require.Empty(entries, "restore must refuse before writing anything under the symlink's target")
}

func TestRestoreRefusesNonEmptyTargetWithoutOverwrite(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(target, "existing.txt"), []byte("x"), 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target})
	require.ErrorContains(err, "not empty")

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)
}

func TestOpenMissingRestoreTargetPinsEveryCreatedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows prevents renaming an open directory handle")
	}
	require := require.New(t)
	base := t.TempDir()
	target := filepath.Join(base, "new", "nested")
	parked := filepath.Join(base, "parked")
	victim := filepath.Join(base, "victim")
	require.NoError(os.Mkdir(victim, 0o700))
	swapped := false

	root, err := openMissingRestoreTarget(target, func(
		parent *os.Root, comp string,
	) (*os.Root, error) {
		next, enterErr := enterRestoreDir(parent, comp)
		if enterErr != nil || swapped {
			return next, enterErr
		}
		swapped = true
		if renameErr := os.Rename(filepath.Join(base, "new"), parked); renameErr != nil {
			_ = next.Close()
			return nil, renameErr
		}
		if symlinkErr := os.Symlink(victim, filepath.Join(base, "new")); symlinkErr != nil {
			_ = next.Close()
			return nil, symlinkErr
		}
		return next, nil
	}, filepath.EvalSymlinks)
	if root != nil {
		require.NoError(root.Close())
	}
	require.Error(err)
	require.ErrorContains(err, "checking restore target")
	_, err = os.Stat(filepath.Join(parked, "nested"))
	require.NoError(err, "creation must remain below the held original parent")
	entries, err := os.ReadDir(victim)
	require.NoError(err)
	require.Empty(entries, "a replacement symlink must not receive created target entries")
}

func TestOpenMissingRestoreTargetPinsExistingAncestorBeforeResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows prevents renaming an open directory handle")
	}
	require := require.New(t)
	base := t.TempDir()
	ancestor := filepath.Join(base, "existing")
	parked := filepath.Join(base, "parked")
	victim := filepath.Join(base, "victim")
	target := filepath.Join(ancestor, "nested")
	require.NoError(os.Mkdir(ancestor, 0o700))
	require.NoError(os.Mkdir(victim, 0o700))

	root, err := openMissingRestoreTarget(target, enterRestoreDir, func(path string) (string, error) {
		require.Equal(ancestor, path)
		require.NoError(os.Rename(ancestor, parked))
		require.NoError(os.Symlink(victim, ancestor))
		return filepath.EvalSymlinks(path)
	})
	if root != nil {
		require.NoError(root.Close())
	}
	require.ErrorContains(err, "was replaced while opening")
	entries, err := os.ReadDir(victim)
	require.NoError(err)
	require.Empty(entries, "a swapped existing ancestor must receive no target entries")
	entries, err = os.ReadDir(parked)
	require.NoError(err)
	require.Empty(entries, "failure must precede creation beneath the detached ancestor")
}

func TestRestoreCoordinatesPinnedTargetBeforeCleanup(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(
		dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	stale := "app.db.restore-" + pack.NewPackID()
	require.NoError(os.WriteFile(filepath.Join(target, stale), []byte("stale"), 0o600))
	acquired := false
	released := false
	coordinator := restoreTargetCoordinatorFunc(func(
		_ context.Context, root *os.Root,
	) (RestoreTargetLease, error) {
		acquired = true
		_, statErr := root.Stat(stale)
		require.NoError(statErr, "coordination must precede stale-target cleanup")
		return restoreTargetLeaseFunc(func() error {
			released = true
			_, statErr := root.Stat("app.db")
			return statErr
		}), nil
	})

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target, Overwrite: true, TargetCoordinator: coordinator,
	})
	require.NoError(err)
	require.True(acquired)
	require.True(released)
	_, err = os.Stat(filepath.Join(target, stale))
	require.ErrorIs(err, os.ErrNotExist)

	blockedTarget := t.TempDir()
	blockedStale := "app.db.restore-" + pack.NewPackID()
	require.NoError(os.WriteFile(
		filepath.Join(blockedTarget, blockedStale), []byte("stale"), 0o600))
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: blockedTarget, Overwrite: true,
		TargetCoordinator: restoreTargetCoordinatorFunc(func(
			context.Context, *os.Root,
		) (RestoreTargetLease, error) {
			return nil, errors.New("target is already owned")
		}),
	})
	require.ErrorContains(err, "target is already owned")
	_, err = os.Stat(filepath.Join(blockedTarget, blockedStale))
	require.NoError(err, "failed coordination must leave existing target bytes untouched")

	if runtime.GOOS != "windows" {
		swappedTarget := t.TempDir()
		swappedHeld := swappedTarget + "-held"
		swappedStale := "app.db.restore-" + pack.NewPackID()
		require.NoError(os.WriteFile(
			filepath.Join(swappedTarget, swappedStale), []byte("stale"), 0o600))
		releasedAfterSwap := false
		_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
			TargetDir: swappedTarget, Overwrite: true,
			TargetCoordinator: restoreTargetCoordinatorFunc(func(
				_ context.Context, _ *os.Root,
			) (RestoreTargetLease, error) {
				require.NoError(os.Rename(swappedTarget, swappedHeld))
				require.NoError(os.Mkdir(swappedTarget, 0o700))
				return restoreTargetLeaseFunc(func() error {
					releasedAfterSwap = true
					return nil
				}), nil
			}),
		})
		require.ErrorContains(err, "was replaced while opening")
		require.True(releasedAfterSwap)
		_, err = os.Stat(filepath.Join(swappedHeld, swappedStale))
		require.NoError(err, "post-lease identity failure must precede stale cleanup")
		replacementEntries, err := os.ReadDir(swappedTarget)
		require.NoError(err)
		require.Empty(replacementEntries, "post-lease identity failure must not stage through the new path")
	}
}

func TestRestoreRechecksEmptyTargetAfterCoordination(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(
		dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	released := false
	coordinator := restoreTargetCoordinatorFunc(func(
		_ context.Context, root *os.Root,
	) (RestoreTargetLease, error) {
		require.NoError(root.WriteFile("raced.txt", []byte("new owner"), 0o600))
		return restoreTargetLeaseFunc(func() error {
			released = true
			return nil
		}), nil
	})

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target, TargetCoordinator: coordinator,
	})
	require.ErrorContains(err, "not empty")
	require.True(released, "failed restore must release target coordination")
	content, readErr := os.ReadFile(filepath.Join(target, "raced.txt"))
	require.NoError(readErr)
	require.Equal("new owner", string(content))
	_, statErr := os.Stat(filepath.Join(target, "app.db"))
	require.ErrorIs(statErr, os.ErrNotExist)
}

// TestRestoreOverwriteRemovesStaleDBSidecars pins the --overwrite hazard: a
// stale -wal/-shm pair or rollback journal left next to the restored database
// would be replayed over the proven bytes on its first normal SQLite open, so
// overwrite must remove them even though it merges the rest of the tree.
func TestRestoreOverwriteRemovesStaleDBSidecars(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	for _, name := range []string{"app.db", "app.db-wal", "app.db-shm", "app.db-journal"} {
		require.NoError(os.WriteFile(filepath.Join(target, name), []byte("stale "+name), 0o600))
	}
	unrelated := filepath.Join(target, "keep-me.txt")
	require.NoError(os.WriteFile(unrelated, []byte("survives the merge"), 0o600))

	res, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)
	for _, name := range []string{"app.db-wal", "app.db-shm", "app.db-journal"} {
		_, err := os.Stat(filepath.Join(target, name))
		require.ErrorIs(err, os.ErrNotExist, "stale sidecar %s must not survive an overwrite restore", name)
	}
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath),
		"the stale database must be fully replaced, not merged")
	_, err = os.Stat(unrelated)
	require.NoError(err, "overwrite merges: unrelated files stay in place")
}

// TestRestoreOverwriteSweepsOrphanedTempFiles pins the crash-recovery sweep: a
// restore killed mid-run leaves a <db>.restore-<ulid> staging temp at the
// target root — and one killed mid-publish leaves <sidecar>.restore-<ulid>
// asides too — and a later --overwrite rerun must remove them rather than
// leave them to accumulate.
func TestRestoreOverwriteSweepsOrphanedTempFiles(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	orphans := []string{
		filepath.Join(target, "app.db.restore-"+pack.NewPackID()),
		filepath.Join(target, "app.db-wal.restore-"+pack.NewPackID()),
		filepath.Join(target, "app.db-journal.restore-"+pack.NewPackID()),
	}
	for _, orphan := range orphans {
		require.NoError(os.WriteFile(orphan, []byte("half-written restore"), 0o600))
	}
	// A file that merely shares the prefix but is not a valid temp name must
	// survive: the sweep is scoped to the exact <name>.restore-<ulid> shapes.
	bystander := filepath.Join(target, "app.db.restore-not-a-ulid")
	require.NoError(os.WriteFile(bystander, []byte("keep me"), 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)

	for _, orphan := range orphans {
		_, statErr := os.Stat(orphan)
		require.ErrorIs(statErr, os.ErrNotExist,
			"orphaned restore temp %s must be swept on overwrite", filepath.Base(orphan))
	}
	_, statErr := os.Stat(bystander)
	require.NoError(statErr, "a non-temp file sharing the prefix must survive")
}

func TestStageRootDatabaseHasNoPackCeilingAndUsesSweepableName(t *testing.T) {
	require := require.New(t)
	target := t.TempDir()
	root, err := openRestoreRoot(target)
	require.NoError(err)
	st := &restoreState{root: root, target: target}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.stageRootDatabase(
		canceled, "app.db", strings.NewReader(""), int64(pack.MaxRawLen)+1)
	require.ErrorIs(err, context.Canceled)
	require.NotContains(err.Error(), "invalid recorded size")

	rel, err := st.stageRootDatabase(
		t.Context(), "app.db", strings.NewReader("sqlite"), int64(len("sqlite")))
	require.NoError(err)
	require.True(strings.HasPrefix(rel, "app.db.restore-"), rel)
	require.NoError(root.Close())

	sweptRoot, err := prepareRestoreTarget(target, true, "app.db")
	require.NoError(err)
	defer func() { _ = sweptRoot.Close() }()
	_, err = sweptRoot.Stat(rel)
	require.ErrorIs(err, os.ErrNotExist)
}

// TestRestoreTargetPathWithURISyntax pins the proof DSN construction: a
// target path containing '?' or '#' must reach SQLite as a path, not be
// misparsed as URI query/fragment syntax.
func TestRestoreTargetPathWithURISyntax(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	// '?' is illegal in Windows filenames, so exercise it only where the
	// filesystem allows it; '#' and space are legal everywhere.
	dirName := "odd? dir#name"
	if runtime.GOOS == "windows" {
		dirName = "odd dir#name"
	}
	target := filepath.Join(t.TempDir(), dirName)
	res, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target})
	require.NoError(err)
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath))
}

// TestRestoredDBDSNDrivePath pins the Windows DSN shape: a drive-letter
// path must be rooted with a slash so SQLite's URI parser reads it as a
// path rather than a URI authority. Windows-only: elsewhere filepath.Abs
// treats a drive-letter path as relative.
func TestRestoredDBDSNDrivePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive-letter paths are only absolute on Windows")
	}
	assert.Equal(t, "file:///C:/Users/x%20y/app.db?immutable=1&mode=ro",
		restoredDBDSN(`C:\Users\x y\app.db`))
}

// TestRestoredDBDSNRelativePath pins that a relative path resolves against
// the working directory instead of being rooted at "/".
func TestRestoredDBDSNRelativePath(t *testing.T) {
	require := require.New(t)
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	require.NoError(err)

	rel := filepath.Join("out", "app.db")
	dsn := restoredDBDSN(rel)
	require.Equal(restoredDBDSN(filepath.Join(cwd, rel)), dsn,
		"relative and absolute forms of the same path must produce the same DSN")
	require.NotContains(dsn, "file:///out",
		"a relative path must not be rooted at /")
}

// TestRestoreRelativeTarget pins that a relative --target works end to end:
// the proof DSN must resolve the restored database against the working
// directory, not root the relative path at "/".
func TestRestoreRelativeTarget(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	t.Chdir(t.TempDir())
	res, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: "restore-out"})
	require.NoError(err)
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath))
}

type callbackUpdateProofApp struct{ App }

func (a callbackUpdateProofApp) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	var userVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return nil, err
	}
	if userVersion != 1 {
		return nil, fmt.Errorf("callback update was not present during restore proof: user_version=%d", userVersion)
	}
	return a.App.RestoredStats(ctx, db)
}

func TestRestoreBeforePublicationUsesPrivateScratchAndPublishesUpdate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(
		dbPath, attachmentsDir, dataDir, t.TempDir(),
	))
	require.NoError(err)

	target := filepath.Join(t.TempDir(), "restore")
	beforeScratch := restorePublicationScratchDirs(t, r)
	res, err := Restore(ctx, r, callbackUpdateProofApp{App: newTestApp()}, RestoreOptions{
		TargetDir: target,
		BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
			assert.Equal(target, staged.TargetDir)
			repositoryStaging, err := filepath.EvalSymlinks(r.Path(stagingDirName))
			require.NoError(err)
			privateDir := filepath.Dir(staged.DBPath)
			scratchRoot := filepath.Dir(privateDir)
			assert.Equal(repositoryStaging, filepath.Dir(scratchRoot),
				"callback scratch must prefer disjoint repository staging")
			require.NoError(safefileio.ValidatePrivateDir(scratchRoot))
			rel, err := filepath.Rel(staged.TargetDir, staged.DBPath)
			require.NoError(err)
			assert.True(rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)),
				"callback database %q must be outside target %q", staged.DBPath, staged.TargetDir)
			assert.FileExists(staged.DBPath)
			assert.NoFileExists(filepath.Join(target, newTestApp().DBFileName()))

			db, err := sql.Open("sqlite3", staged.DBPath)
			if err != nil {
				return err
			}
			if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = 1"); err != nil {
				_ = db.Close()
				return err
			}
			if _, err := db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				_ = db.Close()
				return err
			}
			return db.Close()
		},
	})
	require.NoError(err)

	updated, err := sql.Open("sqlite3", res.DBPath)
	require.NoError(err)
	defer func() { _ = updated.Close() }()
	var userVersion int
	require.NoError(updated.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&userVersion))
	assert.Equal(1, userVersion)
	info, err := os.Stat(res.DBPath)
	require.NoError(err)
	assert.Equal(info.Size(), res.DBBytes)
	assert.Equal(beforeScratch, restorePublicationScratchDirs(t, r))
}

func TestRestoreBeforePublicationScratchIsOutsideRepositoryTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(
		dbPath, attachmentsDir, dataDir, t.TempDir(),
	))
	require.NoError(err)

	hookErr := errors.New("stop after private scratch check")
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: r.Root(),
		Overwrite: true,
		BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
			rel, err := filepath.Rel(staged.TargetDir, staged.DBPath)
			require.NoError(err)
			assert.True(rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)),
				"callback database %q must be outside repository target %q", staged.DBPath, staged.TargetDir)
			return hookErr
		},
	})
	require.ErrorIs(err, hookErr)
	assert.Empty(restoreDatabaseStageFiles(t, r.Root(), newTestApp().DBFileName()))
}

func TestRestoreScratchRejectsTrustedTempInsideTarget(t *testing.T) {
	require := require.New(t)
	target, err := os.OpenRoot(os.TempDir()) //nolint:usetesting // the test exercises the real OS temp directory as a trusted scratch root
	require.NoError(err)
	t.Cleanup(func() { require.NoError(target.Close()) })

	scratch, err := openRestoreScratch(target, t.TempDir())

	require.Error(err)
	require.Nil(scratch)
}

func TestRestoreBeforePublicationFailureLeavesDatabaseUnpublished(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(
		dbPath, attachmentsDir, dataDir, t.TempDir(),
	))
	require.NoError(err)

	target := filepath.Join(t.TempDir(), "restore")
	beforeScratch := restorePublicationScratchDirs(t, r)
	hookErr := errors.New("application publication refused")
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target,
		BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
			assert.Equal(target, staged.TargetDir)
			assert.FileExists(staged.DBPath)
			assert.NoFileExists(filepath.Join(target, newTestApp().DBFileName()))
			return hookErr
		},
	})
	require.ErrorIs(err, hookErr)
	assert.NoFileExists(filepath.Join(target, newTestApp().DBFileName()))
	assert.Empty(restoreDatabaseStageFiles(t, target, newTestApp().DBFileName()))
	assert.Equal(beforeScratch, restorePublicationScratchDirs(t, r))
}

func TestRestoreBeforePublicationCleansReplacementOnCloseFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	target := t.TempDir()
	root, err := openRestoreRoot(target)
	require.NoError(err)
	defer func() { _ = root.Close() }()

	currentRel := "app.db.restore-" + pack.NewPackID()
	require.NoError(os.WriteFile(filepath.Join(target, currentRel), []byte("database"), 0o600))
	st := &restoreState{repo: r, root: root, target: target}
	before := restoreDatabaseStageFiles(t, target, newTestApp().DBFileName())
	closeErr := errors.New("prepared database close failed")
	originalClose := closePreparedRestoreDatabase
	closePreparedRestoreDatabase = func(f *os.File) error {
		require.NoError(f.Close())
		return closeErr
	}
	t.Cleanup(func() { closePreparedRestoreDatabase = originalClose })

	_, _, err = st.prepareBeforePublication(
		t.Context(), currentRel, newTestApp().DBFileName(),
		func(context.Context, RestorePublicationTarget) error { return nil })
	require.ErrorIs(err, closeErr)
	assert.Equal(before, restoreDatabaseStageFiles(t, target, newTestApp().DBFileName()))
}

func TestRestoreBeforePublicationStopsBeforeCallbackWhenCanceled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	target := t.TempDir()
	root, err := openRestoreRoot(target)
	require.NoError(err)
	defer func() { _ = root.Close() }()

	currentRel := "app.db.restore-" + pack.NewPackID()
	require.NoError(os.WriteFile(filepath.Join(target, currentRel), []byte("database"), 0o600))
	st := &restoreState{repo: r, root: root, target: target}
	beforeScratch := restorePublicationScratchDirs(t, r)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	callbackCalled := false

	_, _, err = st.prepareBeforePublication(
		ctx, currentRel, newTestApp().DBFileName(),
		func(context.Context, RestorePublicationTarget) error {
			callbackCalled = true
			return nil
		})

	require.ErrorIs(err, context.Canceled)
	assert.False(callbackCalled)
	assert.Equal(beforeScratch, restorePublicationScratchDirs(t, r))
}

// TestRestoreStatsCheckCatchesManifestMismatchWhenIntegritySkipped proves a
// self-consistent manifest (valid content-derived ID) whose recorded stats
// disagree with the captured pages still fails when integrity_check is omitted.
func TestRestoreStatsCheckCatchesManifestMismatchWhenIntegritySkipped(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	path := r.Path(snapshotsDirName, m.SnapshotID+manifestExt)
	data, err := os.ReadFile(path)
	require.NoError(err)
	var doctored Manifest
	require.NoError(json.Unmarshal(data, &doctored))
	bumped := mustParseStats(t, doctored.Stats)
	bumped.Notes++
	doctored.Stats, err = json.Marshal(bumped)
	require.NoError(err)
	createdAt, err := time.Parse(time.RFC3339, doctored.CreatedAt)
	require.NoError(err)
	forgedID, err := ComputeSnapshotID(createdAt, &doctored)
	require.NoError(err)
	doctored.SnapshotID = forgedID
	out, err := json.MarshalIndent(&doctored, "", "  ")
	require.NoError(err)
	require.NoError(os.Remove(path))
	require.NoError(os.WriteFile(r.Path(snapshotsDirName, forgedID+manifestExt), out, 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir:          filepath.Join(t.TempDir(), "restore"),
		SkipIntegrityCheck: true,
	})
	require.ErrorContains(err, "do not match manifest stats")
}

func TestRestoreCanSkipIntegrityCheck(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	var events []ProgressEvent
	res, err := Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir:          filepath.Join(t.TempDir(), "restore"),
		SkipIntegrityCheck: true,
		Progress:           func(ev ProgressEvent) { events = append(events, ev) },
	})

	require.NoError(err)
	assert.False(res.DatabaseIntegrityChecked)
	final := make(map[ProgressStage]ProgressEvent)
	for _, event := range events {
		if event.Final {
			final[event.Stage] = event
		}
	}
	assert.NotContains(final, ProgressStageIntegrityCheck)
	require.Contains(final, ProgressStageRestoreStats)
	assert.Equal(int64(1), final[ProgressStageRestoreStats].Done)
}

func TestRestoreDetectsCorruptPack(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	packID := m.NewPacks[0]
	path := r.Path("packs", packID[:2], packID+testPackExt)
	data, err := os.ReadFile(path)
	require.NoError(err)
	data[len(data)/3] ^= 0x01
	require.NoError(os.WriteFile(path, data, 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: filepath.Join(t.TempDir(), "restore")})
	require.Error(err, "a corrupted pack must fail the restore, never produce an unverified tree")
}

// TestRestoreJobsSerialMatchesParallel pins the --jobs contract for restore:
// serial and parallel runs produce byte-identical trees.
func TestRestoreJobsSerialMatchesParallel(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	targets := map[int]string{1: filepath.Join(t.TempDir(), "serial"), 8: filepath.Join(t.TempDir(), "parallel")}
	trees := map[int]map[string][32]byte{}
	for jobs, target := range targets {
		_, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Jobs: jobs})
		require.NoError(err)
		trees[jobs] = snapshotDirHashes(t, target)
	}
	require.Equal(trees[1], trees[8])
}

// TestWriteRunRejectsOverflowingBlobOffset pins the subtraction-based bounds
// check: BlobOffset comes from a decoded page-map object, and a huge value
// must produce a restore error, not wrap the addition-based comparison and
// panic on the slice.
func TestWriteRunRejectsOverflowingBlobOffset(t *testing.T) {
	require := require.New(t)
	st := &restoreState{progress: newProgressEmitter(nil)}
	f, err := os.Create(filepath.Join(t.TempDir(), "db"))
	require.NoError(err)
	defer func() { _ = f.Close() }()

	raw := make([]byte, 4096)
	hm := &PageHashMap{PageSize: 4096, PageCount: 1}
	for _, offset := range []uint64{math.MaxUint64 - 100, math.MaxUint64, 4097} {
		err := st.writeRun(f, raw, blobID("b"), PageRun{StartPage: 0, PageCount: 1, BlobOffset: offset}, 4096, hm)
		require.ErrorContains(err, "overruns blob", "offset %d", offset)
	}
}

func TestWriteRootReaderPublishesOnlyExactCompletedContent(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected int64
		cancel   bool
		wantErr  string
	}{
		{name: "exact", content: "verified", expected: 8},
		{name: "short", content: "short", expected: 8, wantErr: "record says 8"},
		{name: "long", content: "too long", expected: 3, wantErr: "record says 3"},
		{name: "canceled", content: "verified", expected: 8, cancel: true, wantErr: context.Canceled.Error()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			target := t.TempDir()
			root, err := openRestoreRoot(target)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(root.Close()) })
			st := &restoreState{root: root, target: target}
			ctx, cancel := context.WithCancel(t.Context())
			if tc.cancel {
				cancel()
			} else {
				defer cancel()
			}
			err = st.writeRootReader(ctx, "content/blob", strings.NewReader(tc.content), tc.expected, 0o600)
			final := filepath.Join(target, "content", "blob")
			if tc.wantErr != "" {
				require.ErrorContains(err, tc.wantErr)
				_, statErr := os.Stat(final)
				require.ErrorIs(statErr, os.ErrNotExist)
				matches, globErr := filepath.Glob(filepath.Join(target, "content", ".restore-*"))
				require.NoError(globErr)
				require.Empty(matches)
				return
			}
			require.NoError(err)
			got, readErr := os.ReadFile(final)
			require.NoError(readErr)
			require.Equal(tc.content, string(got))
		})
	}
}

// TestRestoreRefusesSymlinkEscapeInTarget pins that restore never writes
// through a symlink planted inside the target: with Overwrite set, a symlink at
// a restored extras path that points outside the target must be replaced, not
// followed, so a file outside the target is never truncated or rewritten.
func TestRestoreRefusesSymlinkEscapeInTarget(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	// An extras file rides along in the snapshot (the deletions dir is captured).
	extrasBody := []byte(`{"id":"manifest-1"}`)
	deletionsPath := filepath.Join(dataDir, "deletions", "manifest-1.json")
	require.NoError(os.MkdirAll(filepath.Dir(deletionsPath), 0o700))
	require.NoError(os.WriteFile(deletionsPath, extrasBody, 0o600))

	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	// A victim file outside the restore target.
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	sentinel := []byte("must survive the restore untouched")
	require.NoError(os.WriteFile(victim, sentinel, 0o600))

	// Pre-plant, inside the target, a symlink at the extras path that points at
	// the victim; restore runs with Overwrite so the preexisting tree is merged.
	target := filepath.Join(t.TempDir(), "restore")
	require.NoError(os.MkdirAll(filepath.Join(target, "deletions"), 0o700))
	link := filepath.Join(target, "deletions", "manifest-1.json")
	if err := os.Symlink(victim, link); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)

	got, err := os.ReadFile(victim)
	require.NoError(err)
	require.Equal(sentinel, got,
		"a file outside the target must never be written through a symlink")

	info, err := os.Lstat(link)
	require.NoError(err)
	require.Equal(os.FileMode(0), info.Mode()&os.ModeSymlink,
		"the planted symlink must have been replaced by a real file")
	restored, err := os.ReadFile(link)
	require.NoError(err)
	require.Equal(extrasBody, restored)
}

// TestRestoreOverwritePreflightPreservesTarget pins the destructive-cleanup
// ordering: Overwrite removes the target's database and SQLite sidecars, so
// restore must first prove the source can be materialized (index loads, map
// chains resolve) and the context is live. A broken repository or an
// already-canceled restore must fail with the existing target intact, not
// after its database is already gone.
func TestRestoreOverwritePreflightPreservesTarget(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	existing := map[string][]byte{
		"app.db":     []byte("live database bytes"),
		"app.db-wal": []byte("live wal bytes"),
	}
	for name, body := range existing {
		require.NoError(os.WriteFile(filepath.Join(target, name), body, 0o600))
	}
	checkIntact := func() {
		t.Helper()
		for name, body := range existing {
			got, err := os.ReadFile(filepath.Join(target, name))
			require.NoError(err, "%s must survive a failed preflight", name)
			require.Equal(body, got, "%s must be untouched", name)
		}
	}

	// An already-canceled context must stop before the cleanup.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = Restore(canceled, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.ErrorIs(err, context.Canceled)
	checkIntact()

	// A repository whose blob index is gone must fail preflight the same way.
	indexes := r.Path("indexes")
	trashed := indexes + ".trashed"
	require.NoError(os.Rename(indexes, trashed))
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.Error(err)
	checkIntact()
	require.NoError(os.Rename(trashed, indexes))

	// A repository whose index no longer lists one of the snapshot's page
	// blobs must fail the preflight blob pass — the maps still materialize
	// (their chain blobs are intact), so only the dedicated pass catches it
	// before cleanup.
	known, err := r.LoadBlobIndex()
	require.NoError(err)
	st := &restoreState{repo: r, app: newTestApp(), known: known}
	m, err := r.LatestSnapshot()
	require.NoError(err)
	_, pm, err := st.materializeMaps(m)
	require.NoError(err)
	var remaining []IndexEntry
	for id, e := range known {
		if id != pm.Blobs[0] {
			remaining = append(remaining, e)
		}
	}
	old, err := os.ReadDir(indexes)
	require.NoError(err)
	for _, e := range old {
		require.NoError(os.Remove(filepath.Join(indexes, e.Name())))
	}
	_, err = r.WriteIndex(remaining)
	require.NoError(err)
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.ErrorContains(err, "not present in any index")
	checkIntact()
}

// TestPreflightSnapshotBlobsChecksAllReferences pins the preflight's
// coverage: page blobs, attachment content blobs, and extras file blobs must
// each fail the pass when missing from the index, and an intact snapshot
// must pass cleanly.
func TestPreflightSnapshotBlobsChecksAllReferences(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "deletions"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "deletions", "a.json"), []byte(`{"x":1}`), 0o600))
	m, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)
	require.NotEmpty(m.Extras.Tree, "fixture must carry an extras tree")

	known, err := r.LoadBlobIndex()
	require.NoError(err)
	newState := func() *restoreState {
		return &restoreState{repo: r, app: newTestApp(), known: maps.Clone(known)}
	}

	st := newState()
	_, pm, err := st.materializeMaps(m)
	require.NoError(err)
	require.NoError(st.preflightSnapshotBlobs(t.Context(), m, pm), "an intact snapshot must preflight cleanly")

	refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, testPackExt)
	require.NoError(err)
	require.NotEmpty(refs)
	attachmentID, err := pack.ParseBlobID(refs[0].Hash)
	require.NoError(err)

	treeID, err := pack.ParseBlobID(m.Extras.Tree)
	require.NoError(err)
	rawTree, err := r.ReadBlob(known, treeID, nil, testPackExt)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(rawTree, &tree))
	require.NotEmpty(tree.Entries)
	extrasID, err := pack.ParseBlobID(tree.Entries[0].Blob)
	require.NoError(err)

	for what, missing := range map[string]pack.BlobID{
		"page blob":       pm.Blobs[0],
		"attachment blob": attachmentID,
		"extras blob":     extrasID,
	} {
		st := newState()
		delete(st.known, missing)
		err := st.preflightSnapshotBlobs(t.Context(), m, pm)
		require.ErrorContains(err, what, "missing %s must fail preflight", what)
		require.ErrorContains(err, "not present in any index")
	}
}

// TestPreflightSnapshotBlobsRejectsBadExtrasPaths pins that the preflight
// applies the extras path rules stageExtrasEntry enforces: a tampered tree
// whose paths escape, overlap archive content, or collide must fail before
// the target is touched, not after the database has already been replaced.
func TestPreflightSnapshotBlobsRejectsBadExtrasPaths(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	payload := []byte("payload")
	blob, _, err := appender.Add(payload)
	require.NoError(err)
	entry := func(path string) ExtrasEntry {
		return ExtrasEntry{Path: path, Blob: blob.String(), Size: int64(len(payload))}
	}

	cases := map[string][]ExtrasEntry{
		"escapes the restore target":        {entry("../escape")},
		"overlaps restored archive content": {entry("app.db-wal")},
		"collide under case-folded key":     {entry("tokens/A"), entry("tokens/a")},
		"twice":                             {entry("tokens/a"), entry("tokens/a")},
	}
	trees := make(map[string]pack.BlobID, len(cases))
	for want, entries := range cases {
		raw, err := json.Marshal(&ExtrasTree{Entries: entries})
		require.NoError(err)
		treeID, _, err := appender.Add(raw)
		require.NoError(err)
		trees[want] = treeID
	}
	_, indexEntries, err := appender.Finish()
	require.NoError(err)
	_, err = r.WriteIndex(indexEntries)
	require.NoError(err)

	known, err := r.LoadBlobIndex()
	require.NoError(err)
	for want, treeID := range trees {
		st := &restoreState{repo: r, app: newTestApp(), known: known}
		m := &Manifest{Extras: ManifestExtras{Tree: treeID.String()}}
		err := st.preflightSnapshotBlobs(t.Context(), m, &PageMap{})
		require.ErrorContains(err, want, "tree with %s path must fail preflight", want)
	}
}

// corruptStoredBlob flips one byte inside a blob's stored bytes in its pack
// file, leaving every other blob in the pack readable and the index intact —
// so the index-membership preflight passes and the failure surfaces only
// when restore actually reads the blob.
func corruptStoredBlob(t *testing.T, r *Repo, known map[pack.BlobID]IndexEntry, id pack.BlobID) {
	t.Helper()
	require := require.New(t)
	ie, ok := known[id]
	require.True(ok, "blob %s must be indexed", id)
	packPath := r.Path("packs", ie.PackID[:2], ie.PackID+testPackExt)
	data, err := os.ReadFile(packPath)
	require.NoError(err)
	data[ie.Offset+ie.StoredLen/2] ^= 0x01
	require.NoError(os.WriteFile(packPath, data, 0o600))
}

// seedLiveOverwriteTarget seeds a restore target with a live database and
// WAL and returns the target path plus a check requiring both files
// byte-identical — the assertion every preserves-target test ends with.
func seedLiveOverwriteTarget(t *testing.T) (string, func()) {
	t.Helper()
	target := t.TempDir()
	existing := map[string][]byte{
		"app.db":     []byte("live database bytes"),
		"app.db-wal": []byte("live wal bytes"),
	}
	for name, body := range existing {
		require.NoError(t, os.WriteFile(filepath.Join(target, name), body, 0o600))
	}
	checkIntact := func() {
		t.Helper()
		for name, body := range existing {
			got, err := os.ReadFile(filepath.Join(target, name))
			require.NoError(t, err, "%s must survive a failed restore", name)
			require.Equal(t, body, got, "%s must be untouched", name)
		}
	}
	return target, checkIntact
}

// requireOverwriteRestorePreservesTarget seeds an overwrite target with a
// live database and WAL, runs a restore expected to fail, requires both
// files byte-identical afterward, and returns the restore error for
// message assertions.
func requireOverwriteRestorePreservesTarget(t *testing.T, r *Repo, app App) error {
	t.Helper()
	target, checkIntact := seedLiveOverwriteTarget(t)
	_, err := Restore(t.Context(), r, app,
		RestoreOptions{TargetDir: target, Overwrite: true})
	require.Error(t, err)
	checkIntact()
	return err
}

// TestRestoreOverwritePreservesTargetOnUnreadablePageBlob pins the deferred
// destruction: the index-membership preflight cannot see corrupt pack bytes,
// so a page blob that fails to read mid-materialization must surface before
// publishRestoredDB runs — leaving the Overwrite target's live database and
// WAL exactly as they were.
func TestRestoreOverwritePreservesTargetOnUnreadablePageBlob(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	known, err := r.LoadBlobIndex()
	require.NoError(err)
	st := &restoreState{repo: r, app: newTestApp(), known: known}
	_, pm, err := st.materializeMaps(m)
	require.NoError(err)
	corruptStoredBlob(t, r, known, pm.Blobs[0])
	requireOverwriteRestorePreservesTarget(t, r, newTestApp())
}

// TestRestoreOverwritePreservesTargetOnUnreadableContentBlob pins that the
// database publish waits for the snapshot's content: an attachment or extras
// blob whose pack bytes are corrupt passes the index-membership preflight
// and fails only when read, and that failure must leave an Overwrite
// target's live database and WAL untouched — the restored database must
// never be published ahead of the content it belongs with.
func TestRestoreOverwritePreservesTargetOnUnreadableContentBlob(t *testing.T) {
	for name, pick := range map[string]func(t *testing.T, r *Repo, m *Manifest,
		known map[pack.BlobID]IndexEntry) pack.BlobID{
		"attachment": func(t *testing.T, r *Repo, m *Manifest,
			known map[pack.BlobID]IndexEntry,
		) pack.BlobID {
			t.Helper()
			refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, testPackExt)
			require.NoError(t, err)
			require.NotEmpty(t, refs)
			id, err := pack.ParseBlobID(refs[0].Hash)
			require.NoError(t, err)
			return id
		},
		"extras": func(t *testing.T, r *Repo, m *Manifest,
			known map[pack.BlobID]IndexEntry,
		) pack.BlobID {
			t.Helper()
			require := require.New(t)
			t.Helper()
			require.NotEmpty(m.Extras.Tree, "fixture must carry an extras tree")
			treeID, err := pack.ParseBlobID(m.Extras.Tree)
			require.NoError(err)
			raw, err := r.ReadBlob(known, treeID, nil, testPackExt)
			require.NoError(err)
			var tree ExtrasTree
			require.NoError(json.Unmarshal(raw, &tree))
			require.NotEmpty(tree.Entries)
			id, err := pack.ParseBlobID(tree.Entries[0].Blob)
			require.NoError(err)
			return id
		},
	} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			ctx := t.Context()
			r := initTestRepo(t)
			dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
			require.NoError(os.MkdirAll(filepath.Join(dataDir, "deletions"), 0o700))
			require.NoError(os.WriteFile(
				filepath.Join(dataDir, "deletions", "a.json"), []byte(`{"x":1}`), 0o600))
			m, err := Create(ctx, r, newTestApp(),
				createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
			require.NoError(err)

			known, err := r.LoadBlobIndex()
			require.NoError(err)
			corruptStoredBlob(t, r, known, pick(t, r, m, known))
			requireOverwriteRestorePreservesTarget(t, r, newTestApp())
		})
	}
}

// badStatsApp returns restore-time stats that cannot match any manifest, so
// the restore proof's stats comparison always fails.
type badStatsApp struct{ App }

func (a badStatsApp) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	return json.RawMessage(`{"forged":"stats"}`), nil
}

// TestRestoreOverwritePreservesTargetOnProofFailure pins that the restore
// proof runs against the staging temp BEFORE the database is published: a
// proof failure (here a stats mismatch; integrity_check failures take the
// same path) must leave an Overwrite target's live database and WAL
// untouched, not surface after the swap has already happened.
func TestRestoreOverwritePreservesTargetOnProofFailure(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	err = requireOverwriteRestorePreservesTarget(t, r, badStatsApp{App: newTestApp()})
	require.ErrorContains(err, "do not match manifest stats")
}

// TestRestoreOverwritePreservesSidecarsOnFailedPublish pins the publish
// rollback: the sidecars next to an Overwrite target's live database are set
// aside, not removed, before the final database rename, so a rename that
// fails must put them back — the old database keeps the WAL frames it needs
// — and leave no set-aside debris behind.
func TestRestoreOverwritePreservesSidecarsOnFailedPublish(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	// A directory at the database name makes only the final rename fail (a
	// file cannot replace a directory): every earlier step targets the
	// staging temp, so the restore reaches publishRestoredDB with the
	// sidecars already set aside.
	target := t.TempDir()
	require.NoError(os.Mkdir(filepath.Join(target, "app.db"), 0o700))
	walBody := []byte("live wal bytes")
	require.NoError(os.WriteFile(filepath.Join(target, "app.db-wal"), walBody, 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.ErrorContains(err, "publishing restored database")

	got, err := os.ReadFile(filepath.Join(target, "app.db-wal"))
	require.NoError(err, "the WAL must be renamed back after a failed publish")
	require.Equal(walBody, got, "the WAL must be byte-identical after the rollback")
	entries, err := os.ReadDir(target)
	require.NoError(err)
	for _, e := range entries {
		require.NotContains(e.Name(), ".restore-",
			"a failed publish must leave no staging or set-aside debris")
	}
}

// TestRestoreOverwritePreservesExtrasOnUnreadableExtrasBlob pins extras
// staging: every extras blob is fetched and verified into a temp before any
// extras file is renamed over the target's live one, so a corrupt blob
// discovered on a LATER entry must leave the earlier entries' live files
// untouched too — not just the database.
func TestRestoreOverwritePreservesExtrasOnUnreadableExtrasBlob(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "deletions"), 0o700))
	require.NoError(os.WriteFile(
		filepath.Join(dataDir, "deletions", "a.json"), []byte(`{"new":"a"}`), 0o600))
	require.NoError(os.WriteFile(
		filepath.Join(dataDir, "deletions", "b.json"), []byte(`{"new":"b"}`), 0o600))
	m, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	known, err := r.LoadBlobIndex()
	require.NoError(err)
	treeID, err := pack.ParseBlobID(m.Extras.Tree)
	require.NoError(err)
	raw, err := r.ReadBlob(known, treeID, nil, testPackExt)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(raw, &tree))
	require.Len(tree.Entries, 2, "fixture must stage one entry before the corrupt one")
	last := tree.Entries[len(tree.Entries)-1]
	lastID, err := pack.ParseBlobID(last.Blob)
	require.NoError(err)
	corruptStoredBlob(t, r, known, lastID)

	target, checkIntact := seedLiveOverwriteTarget(t)
	first := tree.Entries[0]
	oldBody := []byte(`{"old":"live"}`)
	require.NoError(os.MkdirAll(filepath.Dir(filepath.Join(target, first.Path)), 0o700))
	require.NoError(os.WriteFile(filepath.Join(target, first.Path), oldBody, 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.Error(err)
	checkIntact()
	got, err := os.ReadFile(filepath.Join(target, first.Path))
	require.NoError(err)
	require.Equal(oldBody, got,
		"an extras entry staged before the corrupt one must not replace the live file")
	require.NoFileExists(filepath.Join(target, last.Path))
	entries, err := os.ReadDir(filepath.Dir(filepath.Join(target, first.Path)))
	require.NoError(err)
	for _, e := range entries {
		require.NotContains(e.Name(), ".restore-",
			"a failed extras stage must remove its staged temps")
	}
}

// droppedPathApp removes one hash from the restored database's derived
// content paths, modeling list/DB divergence that the coverage check must
// catch before the attachment writers run.
type droppedPathApp struct{ App }

func (a droppedPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	for hash := range paths {
		delete(paths, hash)
		break
	}
	return paths, nil
}

// TestRestoreRejectsListedRefWithoutPathBeforeWriting pins that the
// listed-blob coverage check runs before any pack worker is dispatched: a
// listed attachment the restored database records no path for must fail the
// restore before a single content file lands in the target, not from inside
// a worker whose siblings have already published their blobs.
func TestRestoreRejectsListedRefWithoutPathBeforeWriting(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target, checkIntact := seedLiveOverwriteTarget(t)
	_, err = Restore(ctx, r, droppedPathApp{App: newTestApp()},
		RestoreOptions{TargetDir: target, Overwrite: true})
	require.ErrorContains(err, "records no path for it")
	checkIntact()
	require.NoDirExists(filepath.Join(target, "content"),
		"the coverage check must fail before any attachment write")
}

// TestRestoreHonorsCancellationDuringExtras pins the extras stage's
// cancellation point and its ordering against publication: a context
// canceled while extras are staging must stop the run with context.Canceled
// before publishRestoredDB, leaving an Overwrite target's live database and
// WAL untouched and staging nothing further.
func TestRestoreHonorsCancellationDuringExtras(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "deletions"), 0o700))
	require.NoError(os.WriteFile(
		filepath.Join(dataDir, "deletions", "a.json"), []byte(`{"x":1}`), 0o600))
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target, checkIntact := seedLiveOverwriteTarget(t)
	// The staging loop's per-entry check is what stops the stage promptly:
	// without it, extras would keep fetching and staging blobs until some
	// later cancellation point noticed. Staged temps never reach their final
	// paths regardless, so promptness is observed through progress — no
	// entry may report staged after the cancellation the first extras event
	// triggered.
	canceled := false
	stagedAfterCancel := 0
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target, Overwrite: true,
		Progress: func(ev ProgressEvent) {
			if ev.Stage != ProgressStageExtras {
				return
			}
			if canceled && ev.Done > 0 {
				stagedAfterCancel++
			}
			canceled = true
			cancel()
		},
	})
	require.ErrorIs(err, context.Canceled)
	checkIntact()
	require.Zero(stagedAfterCancel,
		"no extras entry may be staged after cancellation")
	require.NoFileExists(filepath.Join(target, "deletions", "a.json"),
		"no extras entry may reach its final path after cancellation")
}

// TestRestoreOverwriteReplacesSymlinkedDB pins that a symlink planted at the
// database name is replaced by the rename, never followed: with the stale-db
// removal deferred to just before the rename, the link now survives until
// that point, and the atomic rename must swap in the real file without ever
// writing through the link.
func TestRestoreOverwriteReplacesSymlinkedDB(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	victim := filepath.Join(t.TempDir(), "victim.db")
	sentinel := []byte("must never be written through the link")
	require.NoError(os.WriteFile(victim, sentinel, 0o600))
	target := t.TempDir()
	if err := os.Symlink(victim, filepath.Join(target, "app.db")); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	res, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)

	got, err := os.ReadFile(victim)
	require.NoError(err)
	require.Equal(sentinel, got, "the symlink's destination must be untouched")
	info, err := os.Lstat(res.DBPath)
	require.NoError(err)
	require.Equal(os.FileMode(0), info.Mode()&os.ModeSymlink,
		"the planted symlink must have been replaced by a real file")
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath))
}

// TestRestoreOverwriteRefusesSymlinkedParentDir pins the in-root symlink
// hazard writeRootFile's verified descent closes: os.Root follows symlinks
// that resolve inside the root, so in overwrite mode a preexisting
// "deletions -> content" link would silently land extras files in the
// already-proven content tree — and the final DB proof never re-hashes
// content files. Every parent component must be a real directory, so the
// restore must fail instead and the content tree stay untouched.
func TestRestoreOverwriteRefusesSymlinkedParentDir(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)

	deletionsPath := filepath.Join(dataDir, "deletions", "manifest-1.json")
	require.NoError(os.MkdirAll(filepath.Dir(deletionsPath), 0o700))
	require.NoError(os.WriteFile(deletionsPath, []byte(`{"id":"manifest-1"}`), 0o600))

	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := filepath.Join(t.TempDir(), "restore")
	require.NoError(os.MkdirAll(filepath.Join(target, "content"), 0o700))
	if err := os.Symlink(filepath.Join(target, "content"), filepath.Join(target, "deletions")); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.ErrorContains(err, "not a real directory")

	_, statErr := os.Lstat(filepath.Join(target, "content", "manifest-1.json"))
	require.ErrorIs(statErr, os.ErrNotExist,
		"the extras file must not be written through the symlinked directory into the content tree")
}

// TestRestoreRefusesSymlinkTargetWithTrailingSeparator pins that the
// symlinked-target guard cannot be sidestepped by addressing the link with a
// trailing separator: POSIX resolves "link/" and "link/." through the symlink
// before lstat, so without normalization the leaf check would report a real
// directory and the whole restore would be redirected into the link's
// destination.
func TestRestoreRefusesSymlinkTargetWithTrailingSeparator(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	require.NoError(os.Mkdir(realDir, 0o700))
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	for _, target := range []string{link, link + "/", link + "/."} {
		_, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target})
		require.ErrorContains(err, "is a symlink", "target %q", target)
	}
	entries, err := os.ReadDir(realDir)
	require.NoError(err)
	require.Empty(entries, "nothing may be restored through the symlink")
}

// TestCheckExtrasCollisionsRejectsAliasingPaths pins the extras pre-pass:
// two entries that resolve to one file — an exact duplicate, a case-folded
// alias, or a lexically distinct spelling of the same cleaned path — must
// fail the restore before anything is written, on every platform.
func TestCheckExtrasCollisionsRejectsAliasingPaths(t *testing.T) {
	require := require.New(t)

	require.NoError(checkExtrasCollisions([]ExtrasEntry{
		{Path: "tokens/a.json"}, {Path: "config.toml"}, {Path: "deletions/x.json"},
	}))

	err := checkExtrasCollisions([]ExtrasEntry{{Path: "tokens/A"}, {Path: "tokens/a"}})
	require.ErrorContains(err, "collide under case-folded key")

	err = checkExtrasCollisions([]ExtrasEntry{{Path: "tokens/a"}, {Path: "tokens/a"}})
	require.ErrorContains(err, `lists path "tokens/a" twice`)

	err = checkExtrasCollisions([]ExtrasEntry{{Path: "tokens/./a"}, {Path: "tokens/a"}})
	require.ErrorContains(err, "collide under case-folded key")

	// Unicode-normalization aliases: APFS and HFS+ resolve the NFC ("é") and
	// NFD ("e" + combining acute) spellings of a name to one file, so the two
	// must collide under the key even though they differ byte-for-byte.
	err = checkExtrasCollisions([]ExtrasEntry{
		{Path: "tokens/caf\u00e9.json"}, {Path: "tokens/cafe\u0301.json"},
	})
	require.ErrorContains(err, "collide under case-folded key")
}

// TestVerifyHeldTargetDetectsReplacedTarget pins the re-verification that
// guards the proof's path-based SQLite opens: once the target directory is
// renamed aside and an impostor directory (with its own database file) is
// planted at the same path, verifyHeldTarget must fail rather than let the
// proof run against a database the held root does not contain.
func TestVerifyHeldTargetDetectsReplacedTarget(t *testing.T) {
	require := require.New(t)
	base := t.TempDir()
	target := filepath.Join(base, "restore")
	require.NoError(os.Mkdir(target, 0o700))
	require.NoError(os.WriteFile(filepath.Join(target, "app.db"), []byte("real"), 0o600))

	root, err := openRestoreRoot(target)
	require.NoError(err)
	defer func() { _ = root.Close() }()
	st := &restoreState{root: root, target: target}

	require.NoError(st.verifyHeldTarget("app.db"),
		"the untouched target must verify against its own root")

	if err := os.Rename(target, filepath.Join(base, "moved-aside")); err != nil {
		// Windows refuses to rename a directory somebody holds open, which
		// also forecloses the replacement this guard detects.
		t.Skip("cannot rename a directory with an open handle on this platform")
	}
	require.NoError(os.Mkdir(target, 0o700))
	require.NoError(os.WriteFile(filepath.Join(target, "app.db"), []byte("impostor"), 0o600))

	err = st.verifyHeldTarget("app.db")
	require.ErrorContains(err, "replaced during restore")

	// A symlink swapped in at the target path must be named as such.
	require.NoError(os.RemoveAll(target))
	if err := os.Symlink(filepath.Join(base, "moved-aside"), target); err != nil {
		t.Skip("symlinks not supported on this platform")
	}
	err = st.verifyHeldTarget("app.db")
	require.ErrorContains(err, "replaced with a symlink")
}

func TestRestoreExtrasEntryRejectsEscapingPaths(t *testing.T) {
	require := require.New(t)
	st := &restoreState{}

	for _, path := range []string{"", "/etc/passwd", "../outside", "a/../../outside", ".."} {
		_, err := st.stageExtrasEntry(t.Context(), newTestApp(), ExtrasEntry{Path: path, Blob: blobID("x").String()})
		require.ErrorContains(err, "escapes the restore target", "path %q", path)
	}
}

// TestRestoreExtrasEntryRejectsArchiveOverlap pins that a tampered extras
// tree cannot overwrite outputs restore already produced and proved: the
// database, its SQLite sidecars, and the attachments tree are off limits,
// case-insensitively (the default macOS filesystem folds case).
func TestRestoreExtrasEntryRejectsArchiveOverlap(t *testing.T) {
	require := require.New(t)
	st := &restoreState{}

	for _, path := range []string{
		"app.db", "APP.DB", "app.db-wal", "app.db-shm", "app.db-journal",
		"content/aa/aa11", "Content/aa/aa11", "content",
	} {
		_, err := st.stageExtrasEntry(t.Context(), newTestApp(), ExtrasEntry{Path: path, Blob: blobID("x").String()})
		require.ErrorContains(err, "overlaps restored archive content", "path %q", path)
	}

	// Traversal that lexically resolves onto a reserved path must be caught
	// too: the raw first segment looks safe, but filepath.Join cleans the
	// ".." away before the write.
	for _, path := range []string{
		"safe/../app.db", "safe/../APP.DB-wal", "safe/../content/aa/aa11",
	} {
		_, err := st.stageExtrasEntry(t.Context(), newTestApp(), ExtrasEntry{Path: path, Blob: blobID("x").String()})
		require.ErrorContains(err, "overlaps restored archive content", "path %q", path)
	}

	// Components ending in a dot or space are rejected outright: on Windows
	// "content." and "content " resolve to the reserved content dir, aliasing
	// it past the folded comparison, and such names are pathological on every
	// platform. The guard fires for the first component and for nested ones.
	for _, path := range []string{
		"content.", "content ", "content./aa/aa11", "app.db.", "app.db ",
		"safe/dir./file", "safe/dir /file",
	} {
		_, err := st.stageExtrasEntry(t.Context(), newTestApp(), ExtrasEntry{Path: path, Blob: blobID("x").String()})
		require.ErrorContains(err, "component ending in a dot or space", "path %q", path)
	}

	// A path that cleans to the target directory itself is rejected outright.
	_, err := st.stageExtrasEntry(t.Context(), newTestApp(), ExtrasEntry{Path: "safe/..", Blob: blobID("x").String()})
	require.ErrorContains(err, "escapes the restore target")

	// Legitimate extras still restore fine (proven end-to-end elsewhere);
	// here just confirm the reserved-name check does not reject them.
	_, err = st.stageExtrasEntry(t.Context(), newTestApp(), ExtrasEntry{Path: "deletions/manifest-1.json", Blob: "not-a-blob"})
	require.NotContains(err.Error(), "overlaps restored archive content")
}

// escapingContentPathApp wraps another App but rewrites every path
// RestoredContentPaths reports to a traversal path, modeling an App
// implementation whose own derivation does not validate what it returns
// (unlike testApp, which does) so this test exercises the engine's own guard
// directly, regardless of which attachment the engine happens to process
// first.
type escapingContentPathApp struct{ App }

func (a escapingContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(paths))
	for hash := range paths {
		out[hash] = []string{"../escape"}
	}
	return out, nil
}

// TestRestoreAttachmentsRejectsEscapingContentPath pins the engine-side guard
// on App.RestoredContentPaths: a path with the same untrusted, restored-DB
// provenance as extras tree entries must be rejected before it is joined
// into the content directory, even when nothing else about the snapshot is
// invalid.
func TestRestoreAttachmentsRejectsEscapingContentPath(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	_, err = Restore(ctx, r, escapingContentPathApp{App: newTestApp()}, RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "restore"),
	})
	require.ErrorContains(err, "escapes the content directory")
	require.ErrorContains(err, "../escape")
}

// extraContentPathApp wraps another App but reports one content hash beyond
// those the snapshot's attachment lists carry, modeling a restored database
// that references a blob no attachment list names.
type extraContentPathApp struct{ App }

func (a extraContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(paths)+1)
	maps.Copy(out, paths)
	out["deadbeefunlisted"] = []string{"de/deadbeefunlisted"}
	return out, nil
}

// TestRestoreRejectsDBHashAbsentFromLists pins the coverage guard: a restored
// database that references a content hash appearing in no attachment list must
// fail the restore rather than silently skip materializing it. Without the
// guard the materialization loop, which iterates listed refs only, would never
// notice the extra reference and restore would report success over a database
// pointing at a missing content file.
func TestRestoreRejectsDBHashAbsentFromLists(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	_, err = Restore(ctx, r, extraContentPathApp{App: newTestApp()}, RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "restore"),
	})
	require.ErrorContains(err, "appears in no attachment list")
	require.ErrorContains(err, "deadbeefunlisted")
}
