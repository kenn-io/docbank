package backup

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

// seedBackupFixture builds a WAL sqlite DB (frozenTestSchema from
// frozen_test.go) plus a matching loose attachments dir, and returns
// (dbPath, attachmentsDir, dataDir, writer).
func seedBackupFixture(t *testing.T) (string, string, string, *sql.DB) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	dataDir := t.TempDir()
	attachmentsDir := filepath.Join(dataDir, "content")
	contentA := []byte("first attachment content")
	contentB := []byte("second attachment content, longer")
	refA := writeLooseAttachment(t, attachmentsDir, contentA)
	refB := writeLooseAttachment(t, attachmentsDir, contentB)

	dbPath := filepath.Join(dataDir, "app.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), frozenTestSchema)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-01-01T00:00:00Z')`)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
		 VALUES (?, ?, ?, '', ''), (?, ?, ?, '', '')`,
		refA.Hash, refA.Hash[:2]+"/"+refA.Hash, refA.Size,
		refB.Hash, refB.Hash[:2]+"/"+refB.Hash, refB.Size)
	require.NoError(err)
	return dbPath, attachmentsDir, dataDir, db
}

func createOpts(dbPath, attachmentsDir, dataDir string, cacheDir string) CreateOptions {
	return CreateOptions{
		DBPath:     dbPath,
		ContentDir: attachmentsDir,
		DataDir:    dataDir,
		Extras:     ExtrasSpec{Dirs: []ExtrasDirSpec{{Name: "deletions"}}},
		CacheDir:   cacheDir,
	}
}

// TestCreateManifestReaderVersionTracksStoragePaths pins the compatibility
// gate for namespaced attachment paths: a snapshot restorable by placing
// every blob at the canonical "<aa>/<hash>" path stays at reader version 1,
// while one whose database records any other path requires the path-aware
// reader — a version-1 restore would report success with the database
// pointing at files that do not exist.
func TestCreateManifestReaderVersionTracksStoragePaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, writer := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	assert.Equal(FormatVersion, m1.FormatVersion)
	assert.Equal(MinReaderVersion, m1.MinReaderVersion,
		"an all-canonical snapshot must stay readable by version-1 readers")

	// The same content hash gains a second row at a namespaced path. The
	// per-hash MIN(storage_path) capture selection stays canonical, so the
	// gate must inspect every row, not the captured refs.
	ref := writeLooseAttachment(t, attachmentsDir, []byte("hash recorded at two paths"))
	_, err = writer.ExecContext(ctx,
		`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
		 VALUES (?, ?, ?, '', ''), (?, ?, ?, '', '')`,
		ref.Hash, ref.Hash[:2]+"/"+ref.Hash, ref.Size,
		ref.Hash, "ns-source/"+ref.Hash[:2]+"/"+ref.Hash, ref.Size)
	require.NoError(err)

	m2, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	assert.Equal(dbPathManifestVersion, m2.FormatVersion)
	assert.Equal(dbPathManifestVersion, m2.MinReaderVersion,
		"a namespaced-path snapshot must be refused by version-1 readers")

	// The current reader restores it, of course.
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: filepath.Join(t.TempDir(), "restore")})
	require.NoError(err)
}

// TestStorePageBlobsJobsSerialMatchesParallel pins the --jobs contract for
// the pack stage: any worker count yields the same page-map delta (blob
// order, run set) and the same readable blobs as a strictly serial run,
// including dedup of identical blob content appearing in two plans.
func TestStorePageBlobsJobsSerialMatchesParallel(t *testing.T) {
	require := require.New(t)
	pageSize := uint32(4096)
	pageCount := uint64(3000)

	path := filepath.Join(t.TempDir(), "pages")
	content := make([]byte, pageCount*uint64(pageSize))
	for p := range pageCount {
		fill := byte(p % 251)
		if p < 256 || (p >= 500 && p < 756) {
			fill = 0 // two identical zero regions: same blob content twice
		}
		start := p * uint64(pageSize)
		for i := range uint64(pageSize) {
			content[start+i] = fill
		}
	}
	require.NoError(os.WriteFile(path, content, 0o600))
	f, err := os.Open(path)
	require.NoError(err)
	defer func() { _ = f.Close() }()

	// Hand-crafted dirty set: two dedicated-blob regions with identical
	// content, plus enough scattered small ranges to split into multiple
	// grouped plans.
	scan := &ScanResult{PageSize: pageSize, PageCount: pageCount}
	scan.Dirty = append(scan.Dirty, PageRange{Start: 0, Count: 256}, PageRange{Start: 500, Count: 256})
	for i := range uint64(800) {
		scan.Dirty = append(scan.Dirty, PageRange{Start: 1000 + i*2, Count: 2})
	}

	run := func(jobs int) (*PageMap, *Repo, []IndexEntry) {
		r := initTestRepo(t)
		appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
		delta, err := storePageBlobs(t.Context(), f, scan, appender, jobs, newProgressEmitter(nil))
		require.NoError(err)
		_, entries, err := appender.Finish()
		require.NoError(err)
		return delta, r, entries
	}
	serial, _, serialEntries := run(1)
	parallel, parallelRepo, parallelEntries := run(8)

	require.Equal(serial, parallel, "the delta must not depend on jobs")
	require.Len(parallelEntries, len(serialEntries))
	require.Less(len(parallel.Blobs), len(parallel.Runs),
		"the duplicate zero regions must share one blob")

	known := map[pack.BlobID]IndexEntry{}
	for _, e := range parallelEntries {
		known[e.Blob] = e
	}
	for _, id := range parallel.Blobs {
		blob, err := parallelRepo.ReadBlob(known, id, nil, testPackExt)
		require.NoError(err)
		require.Equal(id, pack.ComputeBlobID(blob))
	}
}

func TestCreateInitialSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)

	m, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	assert.NotEmpty(m.SnapshotID)
	assert.Empty(m.ParentID)
	assert.Equal("sqlite", m.DB.Engine)
	assert.Zero(m.DB.MapChainDepth, "initial snapshot is a keyframe")
	assert.Positive(m.DB.PageCount)
	assert.Equal([]string{"loose"}, m.Attachments.Layout)
	assert.Equal(int64(2), m.Attachments.Blobs)
	assert.Len(m.Attachments.Lists, 1)
	assert.Empty(m.Attachments.Recipes)
	assert.Equal(int64(2), mustParseStats(t, m.Stats).BlobRows)
	assert.NotEmpty(m.NewPacks)
	assert.NotEmpty(m.NewIndex)
	assert.Positive(m.BytesAdded)
	assert.Equal(newTestApp().ExcludedPaths(), m.Excluded)

	list, err := r.ListSnapshots()
	require.NoError(err)
	require.Len(list, 1)
	assert.Equal(m.SnapshotID, list[0].SnapshotID)

	// Repo config records the page size after the first backup.
	reopened, err := Open(r.Root())
	require.NoError(err)
	assert.Equal(int(m.DB.PageSize), reopened.Config().PageSize)

	// Staging is clean after a successful run.
	entries, err := os.ReadDir(r.Path("staging"))
	require.NoError(err)
	assert.Empty(entries)
}

func TestCreateIncrementalSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	// Mutate: new message and a new attachment.
	contentC := []byte("third attachment added later")
	refC := writeLooseAttachment(t, attachmentsDir, contentC)
	_, err = db.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-02-01T00:00:00Z')`)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
		 VALUES (?, ?, ?, '', '')`,
		refC.Hash, refC.Hash[:2]+"/"+refC.Hash, refC.Size)
	require.NoError(err)

	m2, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	assert.Equal(m1.SnapshotID, m2.ParentID)
	assert.Equal(1, m2.DB.MapChainDepth)
	assert.Equal(int64(3), m2.Attachments.Blobs)
	assert.Len(m2.Attachments.Lists, 2, "parent lists carried over plus one new segment")
	assert.Equal(m1.Attachments.Lists[0], m2.Attachments.Lists[0])
	assert.Less(m2.BytesAdded, m1.BytesAdded, "incremental adds a fraction of the initial bytes")

	// The cache was refreshed to the new snapshot.
	snapID, cached, err := LoadHashMapCache(cacheDir, r.Config().RepoID)
	require.NoError(err)
	assert.Equal(m2.SnapshotID, snapID)
	require.NotNil(cached)
	assert.Equal(m2.DB.PageCount, cached.PageCount)
}

// TestVerifyScanFileIdentityDetectsReplacement pins the freeze/scan identity
// check: the page scanner must read the same file the frozen SQLite session
// opened by path, so a database replaced at its path between the two opens
// fails the capture instead of storing a different file's bytes as page blobs.
func TestVerifyScanFileIdentityDetectsReplacement(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	require.NoError(os.WriteFile(path, []byte("real"), 0o600))
	f, err := os.Open(path)
	require.NoError(err)
	defer func() { _ = f.Close() }()

	require.NoError(verifyScanFileIdentity(f, path))

	impostor := filepath.Join(dir, "impostor.db")
	require.NoError(os.WriteFile(impostor, []byte("fake"), 0o600))
	if err := os.Rename(impostor, path); err != nil {
		// Windows refuses to rename over a file with an open handle, which
		// also forecloses the replacement this guard detects.
		t.Skip("cannot replace an open file on this platform")
	}
	require.ErrorContains(verifyScanFileIdentity(f, path), "was replaced")
}

// TestCreateSameSecondChainOrder covers the bug where two Create calls
// landing in the same wall-clock second produce snapshot IDs whose
// timestamp prefixes tie, leaving lexicographic order (what
// ListSnapshots/LatestSnapshot rely on) to fall back to the uncorrelated
// content-hash suffix. Create must bump CreatedAt so IDs stay strictly
// increasing regardless of wall-clock timing.
func TestCreateSameSecondChainOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	_, err = db.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-02-01T00:00:00Z')`)
	require.NoError(err)
	m2, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	assert.Greater(m2.SnapshotID, m1.SnapshotID, "second snapshot's ID must sort after the first")
	assert.Equal(m1.SnapshotID, m2.ParentID)

	latest, err := r.LatestSnapshot()
	require.NoError(err)
	assert.Equal(m2.SnapshotID, latest.SnapshotID, "LatestSnapshot must return the true latest, not the older tie")

	_, err = db.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)
	m3, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	assert.Greater(m3.SnapshotID, m2.SnapshotID, "chain must keep extending in strictly increasing order")
	assert.Equal(m2.SnapshotID, m3.ParentID, "third snapshot's parent must be the second, not orphaned")
}

// seedPreviewFixture builds a fixture with one content file that also has a
// distinct preview blob, so the manifest counters exercise the preview
// population (content hash UNION preview hash).
func seedPreviewFixture(t *testing.T) (string, string, string) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	dataDir := t.TempDir()
	attachmentsDir := filepath.Join(dataDir, "content")
	refA := writeLooseAttachment(t, attachmentsDir, []byte("content with a preview"))
	refT := writeLooseAttachment(t, attachmentsDir, []byte("preview bytes for A"))

	dbPath := filepath.Join(dataDir, "app.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), frozenTestSchema)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-01-01T00:00:00Z')`)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
		 VALUES (?, ?, ?, ?, ?)`,
		refA.Hash, refA.Hash[:2]+"/"+refA.Hash, refA.Size,
		refT.Hash, refT.Hash[:2]+"/"+refT.Hash)
	require.NoError(err)
	return dbPath, attachmentsDir, dataDir
}

// listUnion decodes a manifest's attachment lists into their ref union.
func listUnion(t *testing.T, r *Repo, m *Manifest) []ContentRef {
	t.Helper()
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, testPackExt)
	require.NoError(t, err)
	return refs
}

// TestCreatePreviewManifestCountersAgree pins Critical 2: a snapshot whose
// content files carry previews must produce manifest counters that agree, so
// full Verify reports no problems. Before the fix, stats.blob_files counted
// only content hashes (excluding previews) while Attachments.Blobs counted the
// captured refs (including previews), and Verify enforced equality between
// them.
func TestCreatePreviewManifestCountersAgree(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir := seedPreviewFixture(t)

	m, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	assert.Equal(int64(2), m.Attachments.Blobs, "one content blob plus one preview blob")
	assert.Equal(m.Attachments.Blobs, mustParseStats(t, m.Stats).BlobFiles, "stats population must match attachments")
	assert.Len(listUnion(t, r, m), 2, "list union must match the manifest counter")

	res, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{})
	require.NoError(err)
	assert.Empty(res.Problems)
}

// TestCreateAttachmentDeletionKeepsVerifyClean pins Critical 1: after a local
// attachment deletion, a fresh snapshot must not inherit a parent list union
// that is a strict superset of the current ref set, or Verify would
// permanently fail otherwise healthy snapshots. Snapshot 2 must carry a fresh
// full list of exactly the surviving refs while snapshot 1 stays verifiable.
func TestCreateAttachmentDeletionKeepsVerifyClean(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Equal(int64(2), m1.Attachments.Blobs)

	// Delete attachment B (highest id row) and its loose file.
	var hashB string
	require.NoError(db.QueryRowContext(t.Context(), `SELECT content_hash FROM blobs ORDER BY id DESC LIMIT 1`).Scan(&hashB))
	_, err = db.ExecContext(t.Context(), `DELETE FROM blobs WHERE content_hash = ?`, hashB)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(attachmentsDir, hashB[:2], hashB)))

	m2, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	var hashA string
	require.NoError(db.QueryRowContext(t.Context(), `SELECT content_hash FROM blobs`).Scan(&hashA))
	require.NotEqual(hashB, hashA)

	assert.Equal(int64(1), m2.Attachments.Blobs, "only the surviving attachment is captured")
	union := listUnion(t, r, m2)
	require.Len(union, 1, "snapshot 2 list union must equal exactly the surviving ref")
	assert.Equal(hashA, union[0].Hash)

	res, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{All: true})
	require.NoError(err)
	assert.Empty(res.Problems, "both snapshots verify cleanly after a deletion")
}

func TestNextCreatedAt(t *testing.T) {
	t.Run("no parent returns now unchanged", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		now := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
		got, err := nextCreatedAt(now, nil)
		require.NoError(err)
		assert.Equal(now, got)
	})

	t.Run("parent strictly before now returns now unchanged", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		now := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
		parent := &Manifest{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)}
		got, err := nextCreatedAt(now, parent)
		require.NoError(err)
		assert.Equal(now, got)
	})

	t.Run("same second as parent bumps by one second", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		parentTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		now := parentTime.Add(500 * time.Millisecond)
		parent := &Manifest{CreatedAt: parentTime.Format(time.RFC3339)}
		got, err := nextCreatedAt(now, parent)
		require.NoError(err)
		assert.Equal(parentTime.Add(time.Second), got)
	})

	t.Run("now before parent (clock skew) still bumps forward", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		parentTime := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)
		now := parentTime.Add(-2 * time.Second)
		parent := &Manifest{CreatedAt: parentTime.Format(time.RFC3339)}
		got, err := nextCreatedAt(now, parent)
		require.NoError(err)
		assert.Equal(parentTime.Add(time.Second), got)
	})

	t.Run("malformed parent created_at is an error", func(t *testing.T) {
		require := require.New(t)
		parent := &Manifest{SnapshotID: "bad-parent", CreatedAt: "not-a-timestamp"}
		_, err := nextCreatedAt(time.Now(), parent)
		require.Error(err)
	})
}

// computeFileHashMap builds a full page-hash map directly from a database
// file, for constructing a self-consistent but wrong parent cache.
func computeFileHashMap(t *testing.T, path string, pageSize uint32) *PageHashMap {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Zero(t, len(data)%int(pageSize), "db file must be a whole number of pages")
	count := uint64(len(data)) / uint64(pageSize)
	hashes := make([]byte, count*pageHashSize)
	for p := range count {
		h := PageHash(data[p*uint64(pageSize) : (p+1)*uint64(pageSize)])
		copy(hashes[p*pageHashSize:], h[:])
	}
	return &PageHashMap{PageSize: pageSize, PageCount: count, Hashes: hashes}
}

// TestCreateRejectsForgedParentHashCache pins the parent-cache authentication:
// a self-consistent local hash-map cache keyed to the true parent snapshot ID
// but carrying the wrong page hashes must not be trusted on the ID match alone.
// Here the forged cache holds the CURRENT database's hashes, so trusting it
// would make the dirty scan see every page as unchanged and silently drop the
// edit from the child's delta — the child would then restore to the parent,
// losing data. The keyframe parent's page-hash-map blob ID authenticates the
// cache and forces a repository rebuild instead.
func TestCreateRejectsForgedParentHashCache(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Zero(m1.DB.MapChainDepth, "the first snapshot is a keyframe")

	// Edit an existing row in place (same-length value) so page content changes
	// but the page count does not, then flush the WAL into the main file.
	_, err = db.ExecContext(t.Context(), `UPDATE notes SET created_at = '2099-09-09T09:09:09Z' WHERE id = 1`)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(err)

	forged := computeFileHashMap(t, dbPath, m1.DB.PageSize)
	require.Equal(m1.DB.PageCount, forged.PageCount,
		"the in-place edit must not change the page count, so geometry alone cannot reject the cache")
	require.NoError(SaveHashMapCache(cacheDir, r.Config().RepoID, m1.SnapshotID, forged))

	m2, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Equal(m1.SnapshotID, m2.ParentID)
	dbAtSnap2, err := os.ReadFile(dbPath)
	require.NoError(err)

	target := filepath.Join(t.TempDir(), "restore")
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{SnapshotID: m2.SnapshotID, TargetDir: target})
	require.NoError(err)
	restored, err := os.ReadFile(filepath.Join(target, "app.db"))
	require.NoError(err)
	require.True(bytes.Equal(dbAtSnap2, restored),
		"the forged parent cache must be rejected so the child snapshot captures the edit")
}

// TestCreateRejectsForgedDeltaParentHashCache pins the delta-parent half of
// the same contract: a delta-chained parent (MapChainDepth > 0) carries no
// full-map digest in the frozen format, so its local hash-map cache cannot be
// authenticated end to end and must be ignored in favor of a repository
// rebuild. Here the parent snapshot is a delta, and the forged cache holds the
// CURRENT database's hashes; trusting it would make the dirty scan see every
// page as unchanged and drop the edit from the child's delta. The repository
// chain rebuild recovers the true parent hashes and captures the edit.
func TestCreateRejectsForgedDeltaParentHashCache(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Zero(m1.DB.MapChainDepth, "the first snapshot is a keyframe")

	// A first in-place edit produces a delta child so the next parent is a
	// delta, not a keyframe.
	_, err = db.ExecContext(t.Context(), `UPDATE notes SET created_at = '2098-08-08T08:08:08Z' WHERE id = 1`)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(err)
	m2, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Positive(m2.DB.MapChainDepth, "the parent of the forged run must be a delta")

	// A second in-place edit is the change the forged cache would hide.
	_, err = db.ExecContext(t.Context(), `UPDATE notes SET created_at = '2099-09-09T09:09:09Z' WHERE id = 1`)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(err)

	forged := computeFileHashMap(t, dbPath, m2.DB.PageSize)
	require.Equal(m2.DB.PageCount, forged.PageCount,
		"the in-place edit must not change the page count, so geometry alone cannot reject the cache")
	require.NoError(SaveHashMapCache(cacheDir, r.Config().RepoID, m2.SnapshotID, forged))

	m3, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Equal(m2.SnapshotID, m3.ParentID)
	dbAtSnap3, err := os.ReadFile(dbPath)
	require.NoError(err)

	target := filepath.Join(t.TempDir(), "restore")
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{SnapshotID: m3.SnapshotID, TargetDir: target})
	require.NoError(err)
	restored, err := os.ReadFile(filepath.Join(target, "app.db"))
	require.NoError(err)
	require.True(bytes.Equal(dbAtSnap3, restored),
		"the forged delta-parent cache must be ignored so the child snapshot captures the edit")
}

// badPackExtApp overrides the pack file extension, for exercising the
// validation at each flow's entry.
type badPackExtApp struct {
	App
	ext string
}

func (a badPackExtApp) PackFileExtension() string { return a.ext }

// TestFlowsRejectBadPackExtension pins validatePackExtension at every flow
// entry: packPath concatenates the extension straight into a path component,
// so a separator- or traversal-carrying extension must fail loudly before
// any pack is written or read, and a multi-dot extension would make
// pack.OpenReader derive the wrong pack ID once packs are encrypted.
func TestFlowsRejectBadPackExtension(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)

	for _, ext := range []string{"", ".", "kpack", ".k/pack", `.k\pack`, "../evil", ".backup.pack", ".k:pack"} {
		app := badPackExtApp{App: newTestApp(), ext: ext}
		_, err := Create(ctx, r, app, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
		require.ErrorContains(err, "pack file extension", "Create ext %q", ext)
		_, err = Verify(ctx, r, app, VerifyOptions{})
		require.ErrorContains(err, "pack file extension", "Verify ext %q", ext)
		_, err = Restore(ctx, r, app, RestoreOptions{TargetDir: filepath.Join(t.TempDir(), "x")})
		require.ErrorContains(err, "pack file extension", "Restore ext %q", ext)
	}
}

// TestCreateHonorsCancellationAfterAttachments pins that Create keeps
// watching ctx once attachment capture — the last inherently ctx-aware stage
// — completes: a cancellation landing there must stop the backup before
// extras capture and sealing, and above all must not publish a manifest for
// a backup the caller canceled.
func TestCreateHonorsCancellationAfterAttachments(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "deletions"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "deletions", "a.json"), []byte("{}"), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := createOpts(dbPath, attachmentsDir, dataDir, t.TempDir())
	opts.Progress = func(ev ProgressEvent) {
		if ev.Stage == ProgressStageAttachments && ev.Final {
			cancel()
		}
	}
	_, err := Create(ctx, r, newTestApp(), opts)
	require.ErrorIs(err, context.Canceled)

	latest, err := r.LatestSnapshot()
	require.NoError(err)
	require.Nil(latest, "no snapshot may be published after cancellation")
}

func TestCreateNoChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	_, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	m2, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	// Only the tiny map/hash delta objects are new; no content re-uploaded.
	assert.Equal(int64(2), m2.Attachments.Blobs)
	assert.Len(m2.Attachments.Lists, 1, "no new list segment when nothing new")
}

// TestCreatePageSizeChangeForcesFullRecapture covers the case where the
// source DB is rebuilt at a different page size between snapshots (e.g. a
// VACUUM with a new PRAGMA page_size). The parent's page/hash-map chain is
// the old page size and can no longer be merged against; Create must fall
// back to a full re-capture instead of hard-erroring on every future run.
func TestCreatePageSizeChangeForcesFullRecapture(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	rebuildAtPageSize(t, db, 8192)

	m2, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	assert.Equal(m1.SnapshotID, m2.ParentID, "lineage still records the true parent snapshot")
	assert.Zero(m2.DB.MapChainDepth, "page-size mismatch forces a keyframe")
	assert.EqualValues(8192, m2.DB.PageSize)
	assert.NotEqual(m1.DB.PageSize, m2.DB.PageSize)

	// The new page map must be a self-contained keyframe: materializing it
	// with no parent chain involved still has to cover every page.
	known, err := r.LoadBlobIndex()
	require.NoError(err)
	fetch := func(id pack.BlobID) ([]byte, error) { return r.ReadBlob(known, id, nil, testPackExt) }
	loaded, err := r.LoadManifest(m2.SnapshotID)
	require.NoError(err)
	chain, err := r.PageMapChain(loaded)
	require.NoError(err)
	assert.Len(chain, 1, "keyframe chain is a single blob")
	full, err := MaterializePageMap(fetch, chain)
	require.NoError(err)
	assert.NoError(full.CheckCoverage())
}

// rebuildAtPageSize rewrites db's on-disk page size via VACUUM, mirroring
// what a `PRAGMA page_size=N; VACUUM;` maintenance pass does to a live
// application database. Page size can only change outside WAL mode, so this
// pins a single connection and toggles the journal mode around the vacuum.
func rebuildAtPageSize(t *testing.T, db *sql.DB, pageSize int) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	conn, err := db.Conn(t.Context())
	require.NoError(err)
	defer func() { require.NoError(conn.Close()) }()

	_, err = conn.ExecContext(t.Context(), `PRAGMA journal_mode=DELETE`)
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), fmt.Sprintf(`PRAGMA page_size=%d`, pageSize))
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), `VACUUM`)
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA journal_mode=WAL`)
	require.NoError(err)
}

func TestCreateHoldsExclusiveLock(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)

	shared, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	defer func() { require.NoError(shared.Release()) }()

	oldTimeout, oldPoll := sharedWaitTimeout, sharedWaitPoll
	sharedWaitTimeout, sharedWaitPoll = 200*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { sharedWaitTimeout, sharedWaitPoll = oldTimeout, oldPoll })

	_, err = Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.ErrorIs(err, ErrRepoLocked)
}

// TestCreatePhasesHonorCancellation pins that the long capture phases
// observe context cancellation instead of running to completion: an
// already-canceled context must surface ctx.Err() from each phase without
// deadlocking its worker pipeline.
func TestCreatePhasesHonorCancellation(t *testing.T) {
	require := require.New(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	pageSize := uint32(4096)
	db := fakeDB(t, pageSize, 64)
	_, err := ScanPages(canceled, bytes.NewReader(db), pageSize, 64, nil, nil)
	require.ErrorIs(err, context.Canceled, "ScanPages")

	path := filepath.Join(t.TempDir(), "pages")
	require.NoError(os.WriteFile(path, db, 0o600))
	f, err := os.Open(path)
	require.NoError(err)
	defer func() { _ = f.Close() }()
	scan := &ScanResult{PageSize: pageSize, PageCount: 64, Dirty: []PageRange{{Start: 0, Count: 64}}}
	r := initTestRepo(t)
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	defer appender.Abort()
	_, err = storePageBlobs(canceled, f, scan, appender, 4, newProgressEmitter(nil))
	require.ErrorIs(err, context.Canceled, "storePageBlobs")

	dir := t.TempDir()
	ref := writeLooseAttachment(t, dir, []byte("attachment content"))
	capAppender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	defer capAppender.Abort()
	_, err = CaptureAttachments(canceled, dir, []ContentRef{ref}, map[string]bool{}, capAppender, CaptureOptions{Jobs: 4})
	require.ErrorIs(err, context.Canceled, "CaptureAttachments")
}
