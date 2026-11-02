package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"

	"go.kenn.io/kit/pack"
)

// VerifyOptions parameterizes one integrity check run (FORMAT.md, Verification Model).
type VerifyOptions struct {
	SnapshotID  string // empty: latest
	All         bool
	Quick       bool
	ForceUnlock bool
	// Jobs is the number of concurrent content-blob read workers used in
	// full mode. Zero or negative selects one worker per CPU. Use 1 to read
	// packs strictly one at a time — the right choice when the repository
	// lives on a spinning disk or NAS share that degrades under concurrent
	// reads. Quick mode reads no content and ignores Jobs.
	Jobs int
	// Progress, if non-nil, receives structured progress events as Verify
	// runs. nil means fully silent. Verify emits events freely and cheaply;
	// throttling for display is a rendering concern of the callback, not
	// Verify's.
	Progress func(ProgressEvent)
}

// Problem names one verification failure precisely (FORMAT.md, Verification Model).
type Problem struct {
	SnapshotID string
	Detail     string
}

// VerifyResult reports what Verify checked and found.
type VerifyResult struct {
	Snapshots    []string
	BlobsChecked int64
	BytesRead    int64
	Problems     []Problem
}

// errBlobUnreadable marks a chain-materialization failure that was already
// recorded as a Problem by verifyState.blob, so callers unwrapping a
// MaterializeHashMap/MaterializePageMap error know not to add a second,
// less specific Problem for the same underlying blob.
var errBlobUnreadable = errors.New("backup: referenced blob failed verification")

// Verify checks a backup repository's integrity (FORMAT.md, Verification Model). It takes
// a SHARED repo lock (released on return): concurrent verifies and restores
// are safe, but a running create/prune (exclusive) is not.
//
// Snapshot selection: All verifies every manifest, SnapshotID verifies one
// (an error if it does not exist), and the default verifies only the latest.
//
// In Quick mode, every referenced blob is resolved through the index and its
// pack footer, but content blobs are not read. The default full mode also
// reads and hash-verifies every content blob, checks the materialized page
// map's coverage, and cross-checks attachment-list totals against the
// manifest. Per-object failures are collected as Problems naming the
// snapshot, blob, and pack; Verify keeps going so every affected snapshot is
// named, rather than stopping at the first Problem.
func Verify(ctx context.Context, r *Repo, app App, opts VerifyOptions) (*VerifyResult, error) {
	if err := validatePackExtension(app.PackFileExtension()); err != nil {
		return nil, err
	}
	lock, err := r.AcquireSharedLock("verify", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	manifests, err := selectManifests(r, opts)
	if err != nil {
		return nil, err
	}
	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}

	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = runtime.GOMAXPROCS(0)
	}
	st := &verifyState{
		ctx:              ctx,
		app:              app,
		repo:             r,
		known:            known,
		quick:            opts.Quick,
		jobs:             jobs,
		readers:          map[string]*pack.Reader{},
		readerErrs:       map[string]error{},
		checked:          map[pack.BlobID]bool{},
		readDone:         map[pack.BlobID]bool{},
		readVerdict:      map[pack.BlobID]string{},
		readLen:          map[pack.BlobID]int64{},
		pendingSet:       map[pack.BlobID]bool{},
		pendingRunChecks: map[pack.BlobID][]pageRunCheck{},
		result:           &VerifyResult{},
		progress:         newProgressEmitter(opts.Progress),
		snapshotTotal:    len(manifests),
	}
	defer st.closeReaders()

	for i, m := range manifests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st.snapshotIndex = i
		if st.quick {
			// Quick mode reads no content, so per-snapshot ticks are the only
			// meaningful granularity.
			st.progress.emit(ProgressEvent{Stage: ProgressStageVerify, Done: int64(i), Total: int64(len(manifests))})
		}
		st.result.Snapshots = append(st.result.Snapshots, m.SnapshotID)
		st.verifySnapshot(m)
		// Full mode's bar advances per content blob inside the drain — for a
		// single-snapshot verify (the default), snapshot-level progress would
		// sit at 0% for the whole run.
		if err := st.drainContentReads(ctx); err != nil {
			return nil, err
		}
		// These run after the drain even when it queued nothing: blobs
		// verified by an earlier snapshot are memoized, but this snapshot's
		// recorded sizes and page-map runs still need checking against them.
		st.checkListedSizes()
		st.checkPageRuns()
	}
	// Full mode's final tick closes out the same cumulative drain counters
	// the bar advanced with; BlobsChecked dedupes blobs shared across
	// snapshots and would not match the bar's denominator under --all.
	finalTotal := st.drainTotal
	if st.quick {
		finalTotal = int64(len(manifests))
	}
	st.progress.emit(ProgressEvent{
		Stage: ProgressStageVerify, Done: finalTotal, Total: finalTotal,
		BytesDone: st.result.BytesRead, Final: true,
	})
	return st.result, nil
}

// selectManifests resolves VerifyOptions' snapshot selection to a manifest
// list, erroring on an empty repository or a missing explicit SnapshotID.
func selectManifests(r *Repo, opts VerifyOptions) ([]*Manifest, error) {
	if opts.All {
		list, err := r.ListSnapshots()
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, errors.New("backup: repository has no snapshots to verify")
		}
		return list, nil
	}
	if opts.SnapshotID != "" {
		m, err := r.LoadManifest(opts.SnapshotID)
		if err != nil {
			return nil, err
		}
		return []*Manifest{m}, nil
	}
	latest, err := r.LatestSnapshot()
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, errors.New("backup: repository has no snapshots to verify")
	}
	return []*Manifest{latest}, nil
}

// maxOpenPackReaders bounds how many pack.Readers Verify or a SnapshotReader keeps open at once.
// A repository can reference thousands of packs; without a bound the reader
// cache would exhaust the process file-descriptor limit (macOS defaults to
// 256). It is a var, not a const, only so tests can lower it. Verify keeps
// reader errors cached for the rest of its run, regardless of eviction.
var maxOpenPackReaders = 64

// verifyState holds the cross-manifest resources a Verify run shares: the
// index map, a bounded LRU open-pack.Reader cache (closed before Verify
// returns), the set of already-checked blobs, and per-blob full-read verdicts
// so a content blob shared by several snapshots is read and hashed only once.
//
// Concurrency model: each snapshot's structural walk (chains, lists, extras,
// metadata reads) runs serially and enqueues its content blobs into
// pendingReads instead of reading them inline; drainContentReads then reads
// them with a pack-grouped worker pool before the next snapshot starts. mu
// guards every field the drain workers touch (result, checked, readDone,
// readVerdict, readerErrs, contentReads, progress emission); the serial
// phases run alone and need no locking.
type verifyState struct {
	recipes        map[pack.BlobID]objectRecipe
	recipeVerdicts map[pack.BlobID]error
	ctx            context.Context
	app            App
	repo           *Repo
	known          map[pack.BlobID]IndexEntry
	quick          bool
	jobs           int
	readers        map[string]*pack.Reader
	readerOrder    []string // LRU order, least-recently-used first
	readerErrs     map[string]error
	checked        map[pack.BlobID]bool
	readDone       map[pack.BlobID]bool
	readVerdict    map[pack.BlobID]string // "" ok, else the cached problem detail
	readLen        map[pack.BlobID]int64  // actual content length of cleanly read blobs
	contentReads   int
	result         *VerifyResult
	progress       *progressEmitter
	mu             sync.Mutex
	// pendingReads/pendingSet queue the current snapshot's content blobs for
	// the drain; pendingSet dedupes repeat references within one snapshot.
	pendingReads []pendingRead
	pendingSet   map[pack.BlobID]bool
	// pendingSizeChecks queues the current snapshot's recorded sizes
	// (attachment list entries and extras tree entries) for comparison
	// against the drained blobs' actual lengths. It is kept separate from
	// pendingReads because reads are memoized across snapshots while every
	// snapshot's recorded sizes must be checked against the blob it
	// references.
	pendingSizeChecks []listedSizeCheck
	// pendingRunChecks queues the current snapshot's page-map runs, keyed by
	// the page blob backing them, for comparison against the drained blobs'
	// actual bytes: run bounds and per-page hashes against the snapshot's
	// page-hash map. Keying by blob lets the drain run a blob's checks while
	// its bytes are still in memory; leftovers (blobs memoized by an earlier
	// snapshot's drain) are re-read afterward by checkPageRuns.
	pendingRunChecks map[pack.BlobID][]pageRunCheck
	// drainDone/drainTotal drive full-mode progress: blobs processed and
	// blobs queued, cumulative across every drain in the run so a multi-
	// snapshot verify never moves backward. Guarded by mu while workers run.
	drainDone  int64
	drainTotal int64
	// snapshotIndex/snapshotTotal are the current snapshot's 0-based position
	// and the total snapshot count being verified, for quick mode's
	// per-snapshot progress ticks.
	snapshotIndex int
	snapshotTotal int
}

// pendingRead is one queued content-blob read attributed to a snapshot.
type pendingRead struct {
	id         pack.BlobID
	snapshotID string
}

// listedSizeCheck is one recorded size — an attachment list entry's or an
// extras tree entry's — checked against the referenced blob's actual content
// length after the drain.
type listedSizeCheck struct {
	id         pack.BlobID
	snapshotID string
	want       int64
	what       string // the referencing record, e.g. `attachment blob <id>`
	source     string // where the size is recorded: "list" or "tree"
}

// pageRunCheck is one page-map run checked against the referenced page blob's
// actual bytes: the run must fit inside the blob, and every page it maps must
// hash to the snapshot's page-hash map entry. hashes is the snapshot's
// materialized page-hash table; nil (hash-map chain failed, already a Problem)
// skips the per-page comparison and checks bounds only.
type pageRunCheck struct {
	snapshotID string
	run        PageRun
	pageSize   uint32
	hashes     []byte
}

func (s *verifyState) closeReaders() {
	for _, pr := range s.readers {
		_ = pr.Close()
	}
}

func (s *verifyState) problem(snapshotID, detail string) {
	s.result.Problems = append(s.result.Problems, Problem{SnapshotID: snapshotID, Detail: detail})
}

// reader opens (or reuses) a cached pack.Reader for packID. OpenReader
// validates the footer checksum, so a damaged footer is caught here; failures
// are cached too, so a pack broken beyond repair is only opened once.
func (s *verifyState) reader(packID string) (*pack.Reader, error) {
	if pr, ok := s.readers[packID]; ok {
		s.touchReader(packID)
		return pr, nil
	}
	if err, ok := s.readerErrs[packID]; ok {
		return nil, err
	}
	pr, err := pack.OpenReader(s.repo.packPath(packID, s.app.PackFileExtension()), nil)
	if err != nil {
		s.readerErrs[packID] = err
		return nil, err
	}
	if len(s.readers) >= maxOpenPackReaders {
		s.evictOldestReader()
	}
	s.readers[packID] = pr
	s.readerOrder = append(s.readerOrder, packID)
	return pr, nil
}

// touchReader moves packID to the most-recently-used end of the LRU order.
func (s *verifyState) touchReader(packID string) {
	for i, id := range s.readerOrder {
		if id == packID {
			s.readerOrder = append(s.readerOrder[:i], s.readerOrder[i+1:]...)
			break
		}
	}
	s.readerOrder = append(s.readerOrder, packID)
}

// evictOldestReader closes and drops the least-recently-used open reader.
func (s *verifyState) evictOldestReader() {
	if len(s.readerOrder) == 0 {
		return
	}
	oldest := s.readerOrder[0]
	s.readerOrder = s.readerOrder[1:]
	if pr, ok := s.readers[oldest]; ok {
		_ = pr.Close()
		delete(s.readers, oldest)
	}
}

// blob resolves one referenced blob and reports whether it checked out.
// readContent must be true for metadata blobs (maps, lists, the extras
// tree): their bytes are how further references are enumerated. Content
// blobs pass readContent=!quick. On success it returns the blob's raw bytes
// when readContent is true. On failure it records a Problem naming the
// snapshot, blob, and pack, and returns ok=false so the caller can skip any
// references that blob would otherwise have named.
func (s *verifyState) blob(id pack.BlobID, snapshotID string, readContent bool) ([]byte, bool) {
	ie, ok := s.known[id]
	if !ok {
		s.problem(snapshotID, fmt.Sprintf("blob %s not present in any index", id))
		return nil, false
	}
	pr, err := s.reader(ie.PackID)
	if err != nil {
		s.problem(snapshotID, fmt.Sprintf("opening pack %s for blob %s: %v", ie.PackID, id, err))
		return nil, false
	}
	entries := pr.Entries()
	var entry *pack.Entry
	for i := range entries {
		if entries[i].ID == id {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		s.problem(snapshotID, fmt.Sprintf("blob %s missing from pack %s footer", id, ie.PackID))
		return nil, false
	}
	if entry.Offset != ie.Offset || entry.StoredLen != ie.StoredLen || entry.Flags != ie.Flags {
		s.problem(snapshotID, fmt.Sprintf(
			"blob %s index entry (offset %d, len %d, flags %#x) disagrees with pack %s footer (offset %d, len %d, flags %#x)",
			id, ie.Offset, ie.StoredLen, ie.Flags, ie.PackID, entry.Offset, entry.StoredLen, entry.Flags))
		return nil, false
	}
	if !s.checked[id] {
		s.checked[id] = true
		s.result.BlobsChecked++
	}
	if !readContent {
		return nil, true
	}
	raw, err := pr.ReadBlob(*entry)
	if err != nil {
		s.problem(snapshotID, fmt.Sprintf("reading blob %s from pack %s: %v", id, ie.PackID, err))
		return nil, false
	}
	if !s.readDone[id] {
		s.readDone[id] = true
		s.result.BytesRead += int64(entry.RawLen)
	}
	return raw, true
}

// verifyContentBlob checks a content blob (page data, attachment, or extras
// file) whose bytes the caller discards. In full mode it memoizes the
// read-and-hash verdict across snapshots: the first snapshot that references a
// blob reads and hashes it, and later snapshots reuse the verdict. A cached
// failure is still re-reported as a Problem naming the referencing snapshot,
// so per-snapshot attribution is preserved. Quick mode does the per-snapshot
// structural check only and never memoizes.
//
// In full mode the read itself is deferred: the blob is queued for
// drainContentReads, which runs after the snapshot's structural walk with a
// pack-grouped worker pool.
func (s *verifyState) verifyContentBlob(id pack.BlobID, snapshotID string) {
	if s.quick {
		s.blob(id, snapshotID, false)
		return
	}
	if detail, seen := s.readVerdict[id]; seen {
		if detail != "" {
			s.problem(snapshotID, detail)
		}
		return
	}
	if s.pendingSet[id] {
		return
	}
	s.pendingSet[id] = true
	s.pendingReads = append(s.pendingReads, pendingRead{id: id, snapshotID: snapshotID})
}

// drainContentReads reads and hash-verifies every content blob the current
// snapshot queued, grouped by pack so each worker reads one pack file at a
// time (sequential within the file), with s.jobs packs in flight at once.
// jobs=1 therefore reads packs strictly one after another — the safe mode
// for repositories on spinning disks. It returns an error only when ctx is
// canceled; per-blob failures become Problems, as in the serial path.
func (s *verifyState) drainContentReads(ctx context.Context) error {
	if len(s.pendingReads) == 0 {
		return ctx.Err()
	}
	groups := map[string][]pendingRead{}
	var order []string
	for _, pr := range s.pendingReads {
		ie, ok := s.known[pr.id]
		if !ok {
			detail := fmt.Sprintf("blob %s not present in any index", pr.id)
			s.problem(pr.snapshotID, detail)
			s.readVerdict[pr.id] = detail
			continue
		}
		if _, seen := groups[ie.PackID]; !seen {
			order = append(order, ie.PackID)
		}
		groups[ie.PackID] = append(groups[ie.PackID], pr)
	}
	s.pendingReads = nil
	clear(s.pendingSet)

	// The full-mode bar tracks content blobs cumulatively across the run's
	// drains: a single-snapshot verify moves smoothly instead of parking at
	// 0/1 snapshots, and an --all run grows the denominator per snapshot
	// instead of jumping back to 0/queued at each drain.
	var queued int64
	for _, g := range groups {
		queued += int64(len(g))
	}
	s.drainTotal += queued
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageVerify, Done: s.drainDone, Total: s.drainTotal, BytesDone: s.result.BytesRead,
	})

	workers := min(s.jobs, len(order))
	packs := make(chan string)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Go(func() {
			for packID := range packs {
				s.verifyPackGroup(packID, groups[packID])
			}
		})
	}
	var ctxErr error
	for _, packID := range order {
		if ctxErr = ctx.Err(); ctxErr != nil {
			break
		}
		// A blocking send would pin a canceled dispatch until a worker frees
		// up; on cancellation the next iteration's check breaks the loop.
		select {
		case packs <- packID:
		case <-ctx.Done():
		}
	}
	close(packs)
	wg.Wait()
	if ctxErr == nil {
		// A cancellation arriving after the last pack was dispatched, while
		// workers were still reading, must surface too: a canceled verify
		// must never report clean success.
		ctxErr = ctx.Err()
	}
	return ctxErr
}

// verifyPackGroup reads one pack's queued content blobs, reproducing the
// structural checks, accounting, and verdict caching of the serial
// blob()+verifyContentBlob path. ReadBlob calls run outside the state lock;
// everything recorded runs under it.
func (s *verifyState) verifyPackGroup(packID string, reads []pendingRead) {
	pr, err := s.openGroupReader(packID)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, rd := range reads {
			detail := fmt.Sprintf("opening pack %s for blob %s: %v", packID, rd.id, err)
			s.problem(rd.snapshotID, detail)
			s.readVerdict[rd.id] = detail
			s.drainDone++
		}
		s.emitDrainProgressLocked()
		return
	}
	defer func() { _ = pr.Close() }()
	entries := pr.Entries()
	entryByID := make(map[pack.BlobID]*pack.Entry, len(entries))
	for i := range entries {
		entryByID[entries[i].ID] = &entries[i]
	}
	for _, rd := range reads {
		s.verifyGroupBlob(pr, packID, entryByID, rd)
	}
}

// openGroupReader opens a dedicated pack.Reader for one drain group. It
// shares the readerErrs cache with the serial path (a pack whose footer is
// broken is reported once per open attempt, not re-parsed per blob) but not
// the LRU reader cache, whose evictions could close a reader another worker
// is using.
func (s *verifyState) openGroupReader(packID string) (*pack.Reader, error) {
	s.mu.Lock()
	if err, ok := s.readerErrs[packID]; ok {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	pr, err := pack.OpenReader(s.repo.packPath(packID, s.app.PackFileExtension()), nil)
	if err != nil {
		s.mu.Lock()
		s.readerErrs[packID] = err
		s.mu.Unlock()
		return nil, err
	}
	return pr, nil
}

// verifyGroupBlob checks one queued blob against its pack: index/footer
// consistency, then a full read whose decode path verifies CRC and the
// content hash against the blob ID.
func (s *verifyState) verifyGroupBlob(
	pr *pack.Reader, packID string, entryByID map[pack.BlobID]*pack.Entry, rd pendingRead,
) {
	fail := func(detail string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.problem(rd.snapshotID, detail)
		s.readVerdict[rd.id] = detail
		s.drainDone++
		s.emitDrainProgressLocked()
	}
	ie := s.known[rd.id]
	entry, ok := entryByID[rd.id]
	if !ok {
		fail(fmt.Sprintf("blob %s missing from pack %s footer", rd.id, packID))
		return
	}
	if entry.Offset != ie.Offset || entry.StoredLen != ie.StoredLen || entry.Flags != ie.Flags {
		fail(fmt.Sprintf(
			"blob %s index entry (offset %d, len %d, flags %#x) disagrees with pack %s footer (offset %d, len %d, flags %#x)",
			rd.id, ie.Offset, ie.StoredLen, ie.Flags, packID, entry.Offset, entry.StoredLen, entry.Flags))
		return
	}
	s.mu.Lock()
	if !s.checked[rd.id] {
		s.checked[rd.id] = true
		s.result.BlobsChecked++
	}
	checks := s.pendingRunChecks[rd.id]
	delete(s.pendingRunChecks, rd.id)
	s.mu.Unlock()
	var raw []byte
	if len(checks) != 0 {
		var err error
		raw, err = pr.ReadBlob(*entry)
		if err != nil {
			fail(fmt.Sprintf("reading blob %s from pack %s: %v", rd.id, packID, err))
			return
		}
	} else {
		ctx := s.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		stream, err := pr.OpenBlob(ctx, *entry)
		if err != nil {
			fail(fmt.Sprintf("opening blob %s from pack %s: %v", rd.id, packID, err))
			return
		}
		_, copyErr := io.Copy(io.Discard, stream)
		if err := errors.Join(copyErr, stream.Close()); err != nil {
			fail(fmt.Sprintf("reading blob %s from pack %s: %v", rd.id, packID, err))
			return
		}
	}
	s.mu.Lock()
	if !s.readDone[rd.id] {
		s.readDone[rd.id] = true
		s.result.BytesRead += int64(entry.RawLen)
	}
	s.readVerdict[rd.id] = ""
	// The read re-derived the blob's SHA-256 identity, so this length is the
	// authenticated content length; checkAttachmentSizes compares listed
	// sizes against it.
	s.readLen[rd.id] = int64(entry.RawLen)
	s.contentReads++
	// Run this blob's queued page-map run checks now, while its bytes are in
	// hand, so page blobs are not re-read after the drain just to hash their
	// pages. Popped under the lock, hashed outside it.
	s.mu.Unlock()

	var details []string
	for _, c := range checks {
		details = append(details, pageRunProblems(rd.id, raw, c)...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, detail := range details {
		s.problem(rd.snapshotID, detail)
	}
	s.drainDone++
	s.emitDrainProgressLocked()
}

// checkListedSizes compares every recorded size the current snapshot queued
// (attachment list entries, extras tree entries) against the referenced
// blob's actual, hash-authenticated content length. Restore refuses a blob
// whose length disagrees with its record, so verify must flag the same
// forgery — a record and manifest whose sizes are wrong but internally
// consistent would otherwise verify cleanly and still be unrestorable. Runs
// after drainContentReads; blobs whose read already failed (or never ran, on
// a canceled drain) are skipped because their read problem is the more
// fundamental report.
func (s *verifyState) checkListedSizes() {
	for _, c := range s.pendingSizeChecks {
		verdict, read := s.readVerdict[c.id]
		if !read || verdict != "" {
			continue
		}
		if got := s.readLen[c.id]; got != c.want {
			s.problem(c.snapshotID, fmt.Sprintf(
				"%s is %d bytes but its %s records %d; restore would refuse this snapshot",
				c.what, got, c.source, c.want))
		}
	}
	s.pendingSizeChecks = s.pendingSizeChecks[:0]
}

// queuePageRunChecks queues every page-map run for comparison against its
// page blob's actual bytes. Restore's writeRun refuses a run whose
// offset+length overruns its blob and any page whose bytes disagree with the
// snapshot's page-hash map, so a forged or corrupted page map or hash map
// must not verify cleanly and then fail restore mid-materialization. Run blob
// indexes were already validated against the blob table during decode. hm is
// nil when the hash-map chain failed (already a Problem); bounds are still
// checked.
func (s *verifyState) queuePageRunChecks(m *Manifest, pm *PageMap, hm *PageHashMap) {
	var hashes []byte
	if hm != nil {
		hashes = hm.Hashes
	}
	for _, run := range pm.Runs {
		id := pm.Blobs[run.BlobIndex]
		s.pendingRunChecks[id] = append(s.pendingRunChecks[id], pageRunCheck{
			snapshotID: m.SnapshotID, run: run, pageSize: pm.PageSize, hashes: hashes,
		})
	}
}

// pageRunProblems validates one queued run against its blob's actual,
// hash-authenticated bytes, mirroring restore's writeRun exactly: the same
// subtraction-based overflow-safe bounds comparison, then each mapped page's
// hash against the snapshot's page-hash map. It returns problem details
// rather than recording them, so drain workers can call it without holding
// the state lock.
func pageRunProblems(id pack.BlobID, raw []byte, c pageRunCheck) []string {
	if raw == nil {
		raw = []byte{}
	}
	length := uint64(c.run.PageCount) * uint64(c.pageSize)
	blobLen := uint64(len(raw))
	if c.run.BlobOffset > blobLen || length > blobLen-c.run.BlobOffset {
		return []string{fmt.Sprintf(
			"page map run (pages %d..%d) overruns blob %s (%d bytes at offset %d); restore would refuse this snapshot",
			c.run.StartPage, c.run.StartPage+uint64(c.run.PageCount)-1, id, blobLen, c.run.BlobOffset)}
	}
	if c.hashes == nil {
		return nil
	}
	segment := raw[c.run.BlobOffset : c.run.BlobOffset+length]
	var problems []string
	for i := range uint64(c.run.PageCount) {
		p := c.run.StartPage + i
		// A run reaching past the hash table means the two maps disagree on
		// geometry, which checkPageMapCoverage/checkHashMapChain already
		// flagged against the manifest; there is no hash to compare here.
		if (p+1)*pageHashSize > uint64(len(c.hashes)) {
			break
		}
		h := PageHash(segment[i*uint64(c.pageSize) : (i+1)*uint64(c.pageSize)])
		if !bytes.Equal(h[:], c.hashes[p*pageHashSize:(p+1)*pageHashSize]) {
			problems = append(problems, fmt.Sprintf(
				"page %d from blob %s does not match the snapshot's page hash map; restore would refuse this snapshot",
				p, id))
			// One mismatched page proves the forgery; reporting every page of
			// a large run would bury the signal.
			break
		}
	}
	return problems
}

// checkPageRuns runs the queued page-run checks that the drain did not
// already handle: runs backed by blobs whose bytes were memoized by an
// earlier snapshot's drain and therefore not re-read this time. Those blobs
// are re-read here (cheap relative to the drain, and only shared blobs under
// --all reach this path). Blobs whose read failed (or never ran, on a
// canceled drain) are skipped: their read problem is the more fundamental
// report.
func (s *verifyState) checkPageRuns() {
	for id, checks := range s.pendingRunChecks {
		verdict, read := s.readVerdict[id]
		if !read || verdict != "" {
			continue
		}
		raw, ok := s.blob(id, checks[0].snapshotID, true)
		if !ok {
			continue
		}
		for _, c := range checks {
			for _, detail := range pageRunProblems(id, raw, c) {
				s.problem(c.snapshotID, detail)
			}
		}
	}
	clear(s.pendingRunChecks)
}

// emitDrainProgressLocked reports the current drain's blob progress and
// cumulative bytes read. Callers must hold s.mu.
func (s *verifyState) emitDrainProgressLocked() {
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageVerify, Done: s.drainDone, Total: s.drainTotal,
		BytesDone: s.result.BytesRead,
	})
}

// fetcher adapts blob to the fetch signature MaterializeHashMap and
// MaterializePageMap expect, so their own chain-walk and decode logic
// enumerates and verifies every chain blob. A blob-level failure (already
// recorded as a Problem by blob) is surfaced as errBlobUnreadable so the
// caller does not add a second, less specific Problem for it.
func (s *verifyState) fetcher(snapshotID string) func(pack.BlobID) ([]byte, error) {
	return func(id pack.BlobID) ([]byte, error) {
		raw, ok := s.blob(id, snapshotID, true)
		if !ok {
			return nil, errBlobUnreadable
		}
		return raw, nil
	}
}

// verifySnapshot enumerates and checks every blob one manifest references
// (FORMAT.md, Verification Model): the page-hash-map and page-map chains, the materialized page
// map's blob table, attachment lists and the content blobs they name, and
// the extras tree and the blobs it names.
func (s *verifyState) verifySnapshot(m *Manifest) {
	recipesOK := s.checkObjectRecipes(m)
	if m.Metadata == nil {
		hashMap := s.checkHashMapChain(m)

		pageMap := s.checkPageMapChain(m)
		if pageMap != nil {
			s.checkPageMapBlobs(m, pageMap)
			if !s.quick {
				s.checkPageMapCoverage(m, pageMap)
				s.queuePageRunChecks(m, pageMap, hashMap)
			}
		}
	} else if recipesOK || m.Metadata.Recipe == "" {
		s.checkPortableMetadata(m)
	}
	s.checkAuxiliary(m)

	refs := s.checkAttachmentLists(m, recipesOK)
	if !s.quick {
		s.checkAttachmentConsistency(m, refs)
	}

	s.checkExtrasTree(m, recipesOK)
}

func (s *verifyState) checkAuxiliary(m *Manifest) {
	for _, artifact := range m.Auxiliary {
		id, err := pack.ParseBlobID(artifact.Blob)
		if err != nil {
			s.problem(m.SnapshotID, fmt.Sprintf(
				"auxiliary artifact %q blob id %q: %v",
				artifact.Name, artifact.Blob, err,
			))
			continue
		}
		if s.quick {
			s.blob(id, m.SnapshotID, false)
			continue
		}
		s.verifyContentBlob(id, m.SnapshotID)
		s.pendingSizeChecks = append(s.pendingSizeChecks, listedSizeCheck{
			id: id, snapshotID: m.SnapshotID, want: artifact.Bytes,
			what:   fmt.Sprintf("auxiliary artifact %q blob %s", artifact.Name, id),
			source: "manifest",
		})
	}
}

func (s *verifyState) checkPortableMetadata(m *Manifest) {
	id, err := pack.ParseBlobID(m.Metadata.Blob)
	if err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("portable metadata blob id %q: %v", m.Metadata.Blob, err))
		return
	}
	if s.checkRecipeSize(id, m.Metadata.Bytes, m.SnapshotID) {
		return
	}
	s.verifyContentBlob(id, m.SnapshotID)
	if !s.quick {
		s.pendingSizeChecks = append(s.pendingSizeChecks, listedSizeCheck{
			id: id, snapshotID: m.SnapshotID, want: m.Metadata.Bytes,
			what: fmt.Sprintf("portable metadata blob %s", id), source: "manifest",
		})
	}
}

// checkHashMapChain enumerates, decodes, and geometry-checks the page-hash-map
// chain, returning the materialized map so queuePageRunChecks can compare page
// blob bytes against it, or nil when the chain failed.
func (s *verifyState) checkHashMapChain(m *Manifest) *PageHashMap {
	chain, err := s.repo.HashMapChain(m)
	if err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("page-hash-map chain: %v", err))
		return nil
	}
	hm, err := MaterializeHashMap(s.fetcher(m.SnapshotID), chain)
	if err != nil {
		if !errors.Is(err, errBlobUnreadable) {
			s.problem(m.SnapshotID, fmt.Sprintf("page-hash-map chain: %v", err))
		}
		return nil
	}
	if s.quick {
		return hm
	}
	if hm.PageCount != m.DB.PageCount {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"page hash map covers %d pages but manifest reports page_count %d", hm.PageCount, m.DB.PageCount))
	}
	if hm.PageSize != m.DB.PageSize {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"page hash map page_size %d disagrees with manifest page_size %d", hm.PageSize, m.DB.PageSize))
	}
	return hm
}

// checkPageMapChain enumerates and decodes the page-map chain, returning the
// materialized map so the caller can check its blob table (and, in full
// mode, its coverage). It returns nil when the chain or any of its blobs
// failed, since downstream checks have nothing usable to work from.
func (s *verifyState) checkPageMapChain(m *Manifest) *PageMap {
	chain, err := s.repo.PageMapChain(m)
	if err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("page-map chain: %v", err))
		return nil
	}
	pageMap, err := MaterializePageMap(s.fetcher(m.SnapshotID), chain)
	if err != nil {
		if !errors.Is(err, errBlobUnreadable) {
			s.problem(m.SnapshotID, fmt.Sprintf("page-map chain: %v", err))
		}
		return nil
	}
	return pageMap
}

func (s *verifyState) checkPageMapBlobs(m *Manifest, pm *PageMap) {
	for _, id := range pm.Blobs {
		s.verifyContentBlob(id, m.SnapshotID)
	}
}

func (s *verifyState) checkPageMapCoverage(m *Manifest, pm *PageMap) {
	if err := pm.CheckCoverage(); err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("page map: %v", err))
	}
	if pm.PageCount != m.DB.PageCount {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"page map covers %d pages but manifest reports page_count %d", pm.PageCount, m.DB.PageCount))
	}
	// Restore refuses this geometry mismatch; verify must flag it too, or a
	// snapshot could verify cleanly and still be unrestorable.
	if pm.PageSize != m.DB.PageSize {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"page map page size %d disagrees with manifest page_size %d", pm.PageSize, m.DB.PageSize))
	}
}

// checkAttachmentLists decodes every attachment list blob the manifest names
// and checks their content when recipes loaded, returning the union of decoded
// refs for the full-mode consistency check even when content cannot be resolved.
func (s *verifyState) checkAttachmentLists(m *Manifest, recipesOK bool) []ContentRef {
	var refs []ContentRef
	for _, listBlob := range m.Attachments.Lists {
		id, err := pack.ParseBlobID(listBlob)
		if err != nil {
			s.problem(m.SnapshotID, fmt.Sprintf("attachment list blob id %q: %v", listBlob, err))
			continue
		}
		raw, ok := s.blob(id, m.SnapshotID, true)
		if !ok {
			continue
		}
		segment, err := DecodeAttachmentList(raw)
		if err != nil {
			s.problem(m.SnapshotID, fmt.Sprintf("attachment list %s: %v", id, err))
			continue
		}
		for _, ref := range segment {
			refs = append(refs, ref)
			contentID, err := pack.ParseBlobID(ref.Hash)
			if err != nil {
				s.problem(m.SnapshotID, fmt.Sprintf("attachment content hash %q: %v", ref.Hash, err))
				continue
			}
			if !recipesOK && len(m.Attachments.Recipes) > 0 {
				// A failed recipe must not look like a missing plain content blob.
				continue
			}
			if s.checkRecipeSize(contentID, ref.Size, m.SnapshotID) {
				continue
			}
			s.verifyContentBlob(contentID, m.SnapshotID)
			if !s.quick {
				s.pendingSizeChecks = append(s.pendingSizeChecks, listedSizeCheck{
					id: contentID, snapshotID: m.SnapshotID, want: ref.Size,
					what: fmt.Sprintf("attachment blob %s", contentID), source: "list",
				})
			}
		}
	}
	return refs
}

// checkAttachmentConsistency cross-checks the union of decoded attachment
// list refs against the manifest's own attachment totals (FORMAT.md, Verification Model).
func (s *verifyState) checkAttachmentConsistency(m *Manifest, refs []ContentRef) {
	var sizeSum int64
	for _, ref := range refs {
		sizeSum += ref.Size
	}
	if int64(len(refs)) != m.Attachments.Blobs {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"attachment list union has %d blobs but manifest reports attachments.blobs %d", len(refs), m.Attachments.Blobs))
	}
	if sizeSum != m.Attachments.BlobBytes {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"attachment list union sums %d bytes but manifest reports attachments.blob_bytes %d", sizeSum, m.Attachments.BlobBytes))
	}
	// App-level manifest consistency (e.g. stats payload vs attachment totals)
	// is opaque to the engine; the app reports any problems it finds.
	for _, detail := range s.app.CheckManifest(m) {
		s.problem(m.SnapshotID, detail)
	}
}

// checkExtrasTree decodes the extras tree blob (if any) and checks every
// blob it names.
func (s *verifyState) checkExtrasTree(m *Manifest, recipesOK bool) {
	if m.Extras.Tree == "" {
		return
	}
	id, err := pack.ParseBlobID(m.Extras.Tree)
	if err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("extras tree blob id %q: %v", m.Extras.Tree, err))
		return
	}
	raw, ok := s.blob(id, m.SnapshotID, true)
	if !ok {
		return
	}
	var tree ExtrasTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("extras tree %s: %v", id, err))
		return
	}
	// Restore refuses escaping, reserved-overlapping, and colliding extras
	// paths and any size mismatch, so verify must flag the same trees — a
	// tampered tree would otherwise verify cleanly and fail restore, possibly
	// after partial materialization. Path and collision checks are structural
	// and run in both modes; sizes are checked against hash-authenticated
	// content lengths in full mode (checkListedSizes).
	if err := checkExtrasCollisions(tree.Entries); err != nil {
		s.problem(m.SnapshotID, strings.TrimPrefix(err.Error(), "backup: "))
	}
	for _, entry := range tree.Entries {
		if _, err := validateExtrasEntryPath(entry.Path, s.app.ContentDirName(), s.app.DBFileName()); err != nil {
			s.problem(m.SnapshotID, strings.TrimPrefix(err.Error(), "backup: "))
		}
		blobID, err := pack.ParseBlobID(entry.Blob)
		if err != nil {
			s.problem(m.SnapshotID, fmt.Sprintf("extras entry %s blob id %q: %v", entry.Path, entry.Blob, err))
			continue
		}
		if !recipesOK && len(m.Extras.Recipes) > 0 {
			// A failed recipe must not look like a missing whole-file blob.
			continue
		}
		if s.checkRecipeSize(blobID, entry.Size, m.SnapshotID) {
			continue
		}
		s.verifyContentBlob(blobID, m.SnapshotID)
		if !s.quick {
			s.pendingSizeChecks = append(s.pendingSizeChecks, listedSizeCheck{
				id: blobID, snapshotID: m.SnapshotID, want: entry.Size,
				what:   fmt.Sprintf("extras entry %q blob %s", entry.Path, blobID),
				source: "tree",
			})
		}
	}
}
