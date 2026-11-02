package backup

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

func newTestVerifyState(t *testing.T, r *Repo, quick bool) *verifyState {
	t.Helper()
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	st := &verifyState{
		app:              newTestApp(),
		repo:             r,
		known:            known,
		quick:            quick,
		jobs:             2,
		readers:          map[string]*pack.Reader{},
		readerErrs:       map[string]error{},
		checked:          map[pack.BlobID]bool{},
		readDone:         map[pack.BlobID]bool{},
		readVerdict:      map[pack.BlobID]string{},
		readLen:          map[pack.BlobID]int64{},
		pendingSet:       map[pack.BlobID]bool{},
		pendingRunChecks: map[pack.BlobID][]pageRunCheck{},
		result:           &VerifyResult{},
	}
	t.Cleanup(st.closeReaders)
	return st
}

func distinctPackIDs(t *testing.T, r *Repo) []string {
	t.Helper()
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	var ids []string
	seen := map[string]bool{}
	for _, ie := range known {
		if !seen[ie.PackID] {
			seen[ie.PackID] = true
			ids = append(ids, ie.PackID)
		}
	}
	return ids
}

func buildVerifyFixture(t *testing.T) (*Repo, *Manifest) {
	t.Helper()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(t, err)
	return r, m
}

func TestVerifyCleanRepo(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r, m := buildVerifyFixture(t)

	for _, quick := range []bool{true, false} {
		res, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{Quick: quick})
		require.NoError(err)
		assert.Empty(res.Problems)
		assert.Equal([]string{m.SnapshotID}, res.Snapshots)
		assert.Positive(res.BlobsChecked)
	}
}

func TestVerifyContinuesAfterBadObjectRecipe(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		name := "sqlite"
		if metadata {
			name = "portable"
		}
		t.Run(name, func(t *testing.T) {
			r, m := buildVerifyFixture(t)
			known, err := r.LoadBlobIndex()
			require.NoError(t, err)
			refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, testPackExt)
			require.NoError(t, err)
			require.NotEmpty(t, refs)

			appender := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
			t.Cleanup(appender.Abort)
			recipe, _, err := appender.Add([]byte("{"))
			require.NoError(t, err)
			tree, err := json.Marshal(ExtrasTree{Entries: []ExtrasEntry{
				{Path: "../escape", Blob: refs[0].Hash, Size: refs[0].Size},
			}})
			require.NoError(t, err)
			treeID, _, err := appender.Add(tree)
			require.NoError(t, err)
			// A recipe-backed logical object has no plain blob in the index.
			refs[0].Hash = blobID("logical attachment").String()
			list, err := EncodeAttachmentList(refs)
			require.NoError(t, err)
			listID, _, err := appender.Add(list)
			require.NoError(t, err)
			_, entries, err := appender.Finish()
			require.NoError(t, err)
			_, err = r.WriteIndex(entries)
			require.NoError(t, err)

			m.FormatVersion, m.MinReaderVersion = chunkedObjectManifestVersion, chunkedObjectManifestVersion
			m.Attachments.Recipes = []string{recipe.String()}
			m.Attachments.Lists = []string{listID.String()}
			m.Extras.Tree = treeID.String()
			if metadata {
				m.DB = ManifestDB{}
				m.Metadata = &ManifestMetadata{
					Format: "test-v1", Blob: refs[0].Hash, Bytes: refs[0].Size, Recipe: recipe.String(),
				}
			}
			id, err := r.WriteManifest(m)
			require.NoError(t, err)

			for _, quick := range []bool{true, false} {
				result, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{SnapshotID: id, Quick: quick})
				require.NoError(t, err)
				var details []string
				for _, problem := range result.Problems {
					assert.Equal(t, id, problem.SnapshotID)
					details = append(details, problem.Detail)
				}
				joined := strings.Join(details, "\n")
				assert.Contains(t, joined, "decoding object recipe")
				assert.Contains(t, joined, "escapes the restore target")
				assert.NotContains(t, joined, refs[0].Hash, "failed recipes must not produce missing plain-blob errors")
			}

			before, err := r.LoadBlobIndex()
			require.NoError(t, err)
			_, err = Prune(t.Context(), r, newTestApp(), PruneOptions{})
			require.ErrorContains(t, err, "cannot prune snapshot")
			require.ErrorContains(t, err, "decoding object recipe")
			after, err := r.LoadBlobIndex()
			require.NoError(t, err)
			assert.Equal(t, before, after)
			for _, packID := range distinctPackIDs(t, r) {
				assert.FileExists(t, r.packPath(packID, testPackExt))
			}
		})
	}
}

func TestVerifySelection(t *testing.T) {
	require := require.New(t)
	r, m := buildVerifyFixture(t)

	res, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{SnapshotID: m.SnapshotID, Quick: true})
	require.NoError(err)
	require.Equal([]string{m.SnapshotID}, res.Snapshots)

	_, err = Verify(t.Context(), r, newTestApp(), VerifyOptions{SnapshotID: "20990101T000000Z-deadbeef"})
	require.Error(err)

	res, err = Verify(t.Context(), r, newTestApp(), VerifyOptions{All: true, Quick: true})
	require.NoError(err)
	require.Equal([]string{m.SnapshotID}, res.Snapshots)
}

func TestVerifyNamesCorruptBlob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r, m := buildVerifyFixture(t)

	// Corrupt one byte in the middle of a pack's blob region.
	packID := m.NewPacks[0]
	path := r.Path("packs", packID[:2], packID+testPackExt)
	data, err := os.ReadFile(path)
	require.NoError(err)
	data[len(data)/3] ^= 0x01
	require.NoError(os.WriteFile(path, data, 0o600))

	res, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{})
	require.NoError(err)
	require.NotEmpty(res.Problems)
	found := false
	for _, p := range res.Problems {
		assert.Equal(m.SnapshotID, p.SnapshotID)
		if assert.NotEmpty(p.Detail) && containsAll(p.Detail, packID) {
			found = true
		}
	}
	assert.True(found, "a problem must name the damaged pack")
}

// TestVerifyJobsSerialMatchesParallel pins the --jobs contract: Jobs=1 (the
// spinning-disk/NAS mode, packs read strictly one at a time) must produce
// the same verdicts, counts, and bytes as the default parallel drain — on a
// clean repository and on one with a corrupted blob.
func TestVerifyJobsSerialMatchesParallel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r, m := buildVerifyFixture(t)

	run := func(jobs int) *VerifyResult {
		res, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{Jobs: jobs})
		require.NoError(err)
		return res
	}
	serial, parallel := run(1), run(8)
	assert.Empty(serial.Problems)
	assert.Empty(parallel.Problems)
	assert.Equal(serial.BlobsChecked, parallel.BlobsChecked)
	assert.Equal(serial.BytesRead, parallel.BytesRead)

	packID := m.NewPacks[0]
	path := r.Path("packs", packID[:2], packID+testPackExt)
	data, err := os.ReadFile(path)
	require.NoError(err)
	data[len(data)/3] ^= 0x01
	require.NoError(os.WriteFile(path, data, 0o600))

	serial, parallel = run(1), run(8)
	require.NotEmpty(serial.Problems)
	require.NotEmpty(parallel.Problems)
	assert.ElementsMatch(serial.Problems, parallel.Problems,
		"serial and parallel drains must report the same problem set")
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestVerifyFlagsPageMapGeometryMismatch pins that verify checks both axes
// of the materialized page map's geometry against the manifest. Restore
// refuses a page-size mismatch, so a verify that missed it would pass a
// snapshot that cannot be restored.
func TestVerifyFlagsPageMapGeometryMismatch(t *testing.T) {
	require := require.New(t)
	r, _ := buildVerifyFixture(t)
	st := newTestVerifyState(t, r, true)

	pm := &PageMap{
		PageSize:  8192,
		PageCount: 4,
		Blobs:     []pack.BlobID{blobID("b")},
		Runs:      []PageRun{{StartPage: 0, PageCount: 4, BlobIndex: 0, BlobOffset: 0}},
	}
	m := &Manifest{SnapshotID: "s", DB: ManifestDB{PageSize: 4096, PageCount: 4}}
	st.checkPageMapCoverage(m, pm)
	require.Len(st.result.Problems, 1)
	require.Contains(st.result.Problems[0].Detail, "page size 8192 disagrees with manifest page_size 4096")
}

// TestVerifyAllProgressIsMonotonic pins full-mode progress across a
// multi-snapshot --all run: each snapshot's drain grows the denominator
// cumulatively, Done never moves backward, and the final event closes out
// the same counters the bar advanced with.
func TestVerifyAllProgressIsMonotonic(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, writer := seedBackupFixture(t)
	cacheDir := t.TempDir()
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	// The second snapshot brings new content blobs, so its drain queues work
	// beyond the first snapshot's.
	_, err = writer.ExecContext(ctx, `INSERT INTO notes (created_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)
	ref := writeLooseAttachment(t, attachmentsDir, []byte("content new to snapshot 2"))
	_, err = writer.ExecContext(ctx,
		`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
		 VALUES (?, ?, ?, '', '')`,
		ref.Hash, ref.Hash[:2]+"/"+ref.Hash, ref.Size)
	require.NoError(err)
	_, err = Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	var events []ProgressEvent
	res, err := Verify(ctx, r, newTestApp(), VerifyOptions{All: true, Progress: func(ev ProgressEvent) {
		if ev.Stage == ProgressStageVerify {
			events = append(events, ev)
		}
	}})
	require.NoError(err)
	require.Empty(res.Problems)
	require.Len(res.Snapshots, 2)

	require.NotEmpty(events)
	prev := ProgressEvent{}
	for _, ev := range events {
		assert.GreaterOrEqual(ev.Done, prev.Done, "Done must never move backward across snapshot drains")
		assert.GreaterOrEqual(ev.Total, prev.Total, "Total only grows as later drains queue work")
		prev = ev
	}
	final := events[len(events)-1]
	require.True(final.Final)
	assert.Equal(final.Total, final.Done, "the final event reports the run complete")
	assert.Greater(final.Total, events[0].Total,
		"the second snapshot's drain must grow the cumulative denominator")
}

func TestVerifyQuickCatchesMissingPack(t *testing.T) {
	require := require.New(t)
	r, m := buildVerifyFixture(t)

	packID := m.NewPacks[0]
	require.NoError(os.Remove(r.Path("packs", packID[:2], packID+testPackExt)))

	res, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{Quick: true})
	require.NoError(err)
	require.NotEmpty(res.Problems)
}

func TestVerifyRefusesUnderExclusiveLock(t *testing.T) {
	require := require.New(t)
	r, _ := buildVerifyFixture(t)

	l, err := r.AcquireExclusiveLock("prune", false)
	require.NoError(err)
	defer func() { require.NoError(l.Release()) }()

	_, err = Verify(t.Context(), r, newTestApp(), VerifyOptions{Quick: true})
	require.ErrorIs(err, ErrRepoLocked)
}

func TestVerifyEmptyRepo(t *testing.T) {
	r := initTestRepo(t)
	_, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{})
	require.Error(t, err)
}

// buildMultiPackRepo creates several snapshots, each adding a new attachment,
// so the repository holds at least the given number of distinct packs.
func buildMultiPackRepo(t *testing.T, minPacks int) *Repo {
	t.Helper()
	require := require.New(t)
	t.Helper()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()
	_, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	for i := 0; len(distinctPackIDs(t, r)) < minPacks; i++ {
		require.Less(i, 20, "expected distinct packs to accumulate")
		content := []byte(strings.Repeat("x", i+1) + "-attachment")
		ref := writeLooseAttachment(t, attachmentsDir, content)
		_, err = db.ExecContext(t.Context(),
			`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
			 VALUES (?, ?, ?, '', '')`,
			ref.Hash, ref.Hash[:2]+"/"+ref.Hash, ref.Size)
		require.NoError(err)
		_, err = Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
		require.NoError(err)
	}
	return r
}

// TestVerifyReaderCacheEvictsAndReopens pins Important 5: the pack-reader
// cache is a bounded LRU that closes the least-recently-used reader on
// overflow and reopens a fresh one when the pack is requested again.
func TestVerifyReaderCacheEvictsAndReopens(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	old := maxOpenPackReaders
	maxOpenPackReaders = 2
	t.Cleanup(func() { maxOpenPackReaders = old })

	r := buildMultiPackRepo(t, 3)
	packs := distinctPackIDs(t, r)
	require.GreaterOrEqual(len(packs), 3)

	st := newTestVerifyState(t, r, false)
	p0, err := st.reader(packs[0])
	require.NoError(err)
	_, err = st.reader(packs[1])
	require.NoError(err)
	require.Len(st.readers, 2)

	_, err = st.reader(packs[2]) // over the cap: evicts packs[0]
	require.NoError(err)
	assert.Len(st.readers, 2)
	_, present := st.readers[packs[0]]
	assert.False(present, "LRU evicted the least-recently-used reader")

	// The evicted reader was closed: reading through the stale handle fails.
	entries := p0.Entries()
	require.NotEmpty(entries)
	_, err = p0.ReadBlob(entries[0])
	require.Error(err, "evicted reader must be closed")

	// Re-requesting the evicted pack reopens a fresh reader.
	p0b, err := st.reader(packs[0])
	require.NoError(err)
	assert.NotSame(p0, p0b, "re-request reopened a new reader instance")
	assert.Len(st.readers, 2)
	_, present = st.readers[packs[1]]
	assert.False(present, "reopening evicted the next least-recently-used reader")
}

// TestVerifyMemoizesSharedContentReads pins Important 6: a content blob shared
// by several snapshots is fully read once; later snapshots reuse the verdict.
func TestVerifyMemoizesSharedContentReads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	// No source changes: snapshot 2 shares every content blob with snapshot 1.
	m2, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	ctx := t.Context()
	st1 := newTestVerifyState(t, r, false)
	st1.verifySnapshot(m1)
	require.NoError(st1.drainContentReads(ctx))
	require.Empty(st1.result.Problems)
	assert.Positive(st1.contentReads)

	st2 := newTestVerifyState(t, r, false)
	st2.verifySnapshot(m1)
	require.NoError(st2.drainContentReads(ctx))
	st2.verifySnapshot(m2)
	require.NoError(st2.drainContentReads(ctx))
	require.Empty(st2.result.Problems)
	assert.Equal(st1.contentReads, st2.contentReads,
		"snapshot 2 shares all content, so memoization adds no new full reads")
}

// TestVerifyFlagsForgedAttachmentListSize pins that full verify compares each
// listed attachment size against the blob's hash-authenticated content
// length: restore refuses a size mismatch, so a forged-but-internally-
// consistent list must not verify cleanly. The forged size check is injected
// at the state level because a real forgery would require recomputing pack,
// index, and manifest identities wholesale.
func TestVerifyFlagsForgedAttachmentListSize(t *testing.T) {
	require := require.New(t)
	r, m := buildVerifyFixture(t)
	known, err := r.LoadBlobIndex()
	require.NoError(err)
	refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, newTestApp().PackFileExtension())
	require.NoError(err)
	require.NotEmpty(refs)

	ctx := t.Context()
	st := newTestVerifyState(t, r, false)
	st.verifySnapshot(m)
	require.NoError(st.drainContentReads(ctx))
	st.checkListedSizes()
	require.Empty(st.result.Problems, "honest sizes must verify cleanly")

	id, err := pack.ParseBlobID(refs[0].Hash)
	require.NoError(err)
	st.pendingSizeChecks = append(st.pendingSizeChecks, listedSizeCheck{
		id: id, snapshotID: m.SnapshotID, want: refs[0].Size + 1,
		what: "attachment blob " + id.String(), source: "list",
	})
	st.checkListedSizes()
	require.Len(st.result.Problems, 1)
	require.Contains(st.result.Problems[0].Detail, "but its list records")
	require.Empty(st.pendingSizeChecks, "processed checks must not leak into the next snapshot")
}

// TestVerifyFlagsBadExtrasTree pins restore/verify parity for extras trees:
// escaping paths, reserved-name overlaps, case-folded path collisions, and
// recorded sizes that disagree with the blob's actual content length all
// make restore refuse the snapshot, so verify must flag each of them rather
// than pass a tree that later fails restore after partial materialization.
func TestVerifyFlagsBadExtrasTree(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	payload := []byte("payload")
	blob, _, err := appender.Add(payload)
	require.NoError(err)
	tree := ExtrasTree{Entries: []ExtrasEntry{
		{Path: "../escape", Blob: blob.String(), Size: int64(len(payload))},
		{Path: "app.db", Blob: blob.String(), Size: int64(len(payload))},
		{Path: "tokens/A", Blob: blob.String(), Size: int64(len(payload))},
		{Path: "tokens/a", Blob: blob.String(), Size: int64(len(payload))},
		{Path: "ok/file.json", Blob: blob.String(), Size: int64(len(payload)) + 1},
	}}
	raw, err := json.Marshal(&tree)
	require.NoError(err)
	treeID, _, err := appender.Add(raw)
	require.NoError(err)
	_, entries, err := appender.Finish()
	require.NoError(err)
	_, err = r.WriteIndex(entries)
	require.NoError(err)

	st := newTestVerifyState(t, r, false)
	m := &Manifest{SnapshotID: "test-snapshot", Extras: ManifestExtras{Tree: treeID.String()}}
	st.checkExtrasTree(m, true)
	require.NoError(st.drainContentReads(t.Context()))
	st.checkListedSizes()

	var details []string
	for _, p := range st.result.Problems {
		details = append(details, p.Detail)
	}
	joined := strings.Join(details, "\n")
	require.Contains(joined, "escapes the restore target")
	require.Contains(joined, "overlaps restored archive content")
	require.Contains(joined, "collide under case-folded key")
	require.Contains(joined, `extras entry "ok/file.json"`)
	require.Contains(joined, "but its tree records")
}

// TestVerifyFlagsOverrunningPageRun pins restore/verify parity for page-map
// run bounds: restore's writeRun refuses a run whose offset+length overruns
// its blob, so verify must flag such a run instead of passing a snapshot that
// fails mid-restore. The forged run is injected at the state level (a real
// forgery would require recomputing map, index, and manifest identities);
// queuePageRunChecks over honest snapshots is exercised by every clean full
// verify. The huge-offset case pins the overflow-safe comparison.
func TestVerifyFlagsOverrunningPageRun(t *testing.T) {
	require := require.New(t)
	r, m := buildVerifyFixture(t)

	ctx := t.Context()
	st := newTestVerifyState(t, r, false)
	st.verifySnapshot(m)
	require.NoError(st.drainContentReads(ctx))
	st.checkListedSizes()
	st.checkPageRuns()
	require.Empty(st.result.Problems, "an honest snapshot's runs must verify cleanly")

	pm := st.checkPageMapChain(m)
	require.NotNil(pm)
	require.NotEmpty(pm.Runs)
	blob := pm.Blobs[pm.Runs[0].BlobIndex]
	for _, offset := range []uint64{uint64(st.readLen[blob]) + 1, math.MaxUint64 - 100} {
		st.pendingRunChecks[blob] = append(st.pendingRunChecks[blob], pageRunCheck{
			snapshotID: m.SnapshotID, pageSize: pm.PageSize,
			run: PageRun{StartPage: 0, PageCount: 1, BlobOffset: offset},
		})
		st.checkPageRuns()
		require.NotEmpty(st.result.Problems, "offset %d", offset)
		require.Contains(st.result.Problems[len(st.result.Problems)-1].Detail, "overruns blob")
		require.Empty(st.pendingRunChecks)
	}
}

// TestVerifyDrainSurfacesLateCancellation pins the drain's cancellation
// contract: a context canceled after the last pack was dispatched, while a
// worker is still reading, must surface from drainContentReads — a canceled
// single-snapshot verify would otherwise return clean success. One queued
// blob makes the timing deterministic: dispatch has necessarily finished
// before the worker's per-blob progress event (Done > 0) triggers the cancel.
func TestVerifyDrainSurfacesLateCancellation(t *testing.T) {
	require := require.New(t)
	r, m := buildVerifyFixture(t)

	st := newTestVerifyState(t, r, false)
	pm := st.checkPageMapChain(m)
	require.NotNil(pm)
	require.NotEmpty(pm.Blobs)
	require.Empty(st.result.Problems)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	st.progress = newProgressEmitter(func(ev ProgressEvent) {
		if ev.Done > 0 {
			cancel()
		}
	})
	st.verifyContentBlob(pm.Blobs[0], m.SnapshotID)
	require.ErrorIs(st.drainContentReads(ctx), context.Canceled)
}

// TestVerifyFlagsPageHashMapMismatch pins restore/verify parity for page
// content: restore's writeRun hash-verifies every materialized page against
// the snapshot's page-hash map and refuses on mismatch, so a snapshot whose
// hash map disagrees with its page-map bytes (a forged hash-map object, or a
// buggy writer) must not verify cleanly and then fail restore. The forged
// hashes are injected at the state level, as in the overrun test, and both
// check paths are exercised: the drain (bytes in hand) and the post-drain
// sweep (memoized blob re-read).
func TestVerifyFlagsPageHashMapMismatch(t *testing.T) {
	require := require.New(t)
	r, m := buildVerifyFixture(t)
	ctx := t.Context()

	st := newTestVerifyState(t, r, false)
	hm := st.checkHashMapChain(m)
	require.NotNil(hm)
	pm := st.checkPageMapChain(m)
	require.NotNil(pm)
	require.NotEmpty(pm.Runs)
	require.Empty(st.result.Problems)
	run := pm.Runs[0]
	blob := pm.Blobs[run.BlobIndex]
	forged := append([]byte(nil), hm.Hashes...)
	forged[run.StartPage*pageHashSize] ^= 0xff

	// Drain path: the check is queued before the blob's first read, so
	// verifyGroupBlob pops it while the bytes are in memory.
	st.pendingRunChecks[blob] = append(st.pendingRunChecks[blob], pageRunCheck{
		snapshotID: m.SnapshotID, run: run, pageSize: pm.PageSize, hashes: forged,
	})
	st.verifyContentBlob(blob, m.SnapshotID)
	require.NoError(st.drainContentReads(ctx))
	require.Len(st.result.Problems, 1)
	require.Contains(st.result.Problems[0].Detail, "does not match the snapshot's page hash map")

	// Sweep path: the blob is already memoized, so checkPageRuns must re-read
	// it and reach the same verdict a later snapshot's restore would.
	st.pendingRunChecks[blob] = append(st.pendingRunChecks[blob], pageRunCheck{
		snapshotID: m.SnapshotID, run: run, pageSize: pm.PageSize, hashes: forged,
	})
	st.checkPageRuns()
	require.Len(st.result.Problems, 2)
	require.Contains(st.result.Problems[1].Detail, "does not match the snapshot's page hash map")
	require.Empty(st.pendingRunChecks)

	// The honest hashes must still pass through both paths.
	st.pendingRunChecks[blob] = append(st.pendingRunChecks[blob], pageRunCheck{
		snapshotID: m.SnapshotID, run: run, pageSize: pm.PageSize, hashes: hm.Hashes,
	})
	st.checkPageRuns()
	require.Len(st.result.Problems, 2, "honest page hashes must not be flagged")
}

// TestVerifyDetectsHashMapGeometryMismatch pins Minor 11: full verify compares
// the materialized page-hash-map's geometry against the manifest. A simple
// byte-tamper of the manifest is now rejected earlier by LoadManifest's
// content-derived ID check, so this models the remaining reachable case — a
// buggy writer that recorded the wrong geometry and computed a self-consistent
// snapshot ID over it.
func TestVerifyDetectsHashMapGeometryMismatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r, m := buildVerifyFixture(t)

	path := r.Path(snapshotsDirName, m.SnapshotID+manifestExt)
	data, err := os.ReadFile(path)
	require.NoError(err)
	var doctored Manifest
	require.NoError(json.Unmarshal(data, &doctored))
	doctored.DB.PageCount++
	createdAt, err := time.Parse(time.RFC3339, doctored.CreatedAt)
	require.NoError(err)
	forgedID, err := ComputeSnapshotID(createdAt, &doctored)
	require.NoError(err)
	doctored.SnapshotID = forgedID
	out, err := json.MarshalIndent(&doctored, "", "  ")
	require.NoError(err)
	require.NoError(os.Remove(path))
	require.NoError(os.WriteFile(r.Path(snapshotsDirName, forgedID+manifestExt), out, 0o600))

	res, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{})
	require.NoError(err)
	found := false
	for _, p := range res.Problems {
		if strings.Contains(p.Detail, "page hash map covers") {
			found = true
		}
	}
	assert.True(found, "full verify must flag the hash-map geometry mismatch")
}
