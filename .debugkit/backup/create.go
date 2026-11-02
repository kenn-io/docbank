package backup

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"go.kenn.io/kit/pack"
)

// CreateOptions parameterizes one snapshot capture.
type CreateOptions struct {
	DBPath     string
	ContentDir string
	// SQLiteOpener selects the SQLite implementation used for page-map
	// capture. Nil preserves Kit's mattn/go-sqlite3 default.
	SQLiteOpener SQLiteOpener
	// MetadataSource selects portable application metadata instead of SQLite
	// page capture. DBPath is ignored in this mode.
	MetadataSource MetadataSource
	// ContentSource, when non-nil, supplies attachment content bytes during
	// capture instead of the engine reading ContentDir; ContentDir is then
	// ignored for content reads (it may be empty). Extras and the page scan
	// are unaffected. See ContentSource.
	ContentSource ContentSource
	// DataDir anchors the Extras spec's directory walks and glob matches.
	DataDir string
	// Extras selects the operational files that ride along with the
	// snapshot. The engine imposes no default set; the application supplies
	// an explicit spec (ExtrasSpec).
	Extras                ExtrasSpec
	AllowPlaintextSecrets bool
	// IncludeConfig/IncludeTokens are recorded verbatim into the manifest's
	// options (wire-frozen fields, FORMAT.md); they select nothing by
	// themselves — the Extras spec does.
	IncludeConfig bool
	IncludeTokens bool
	Tag           string
	ZstdLevel     int
	CacheDir      string
	Freezer       FreezeCoordinator
	ForceUnlock   bool
	// Jobs is the number of concurrent attachment read+compress workers.
	// Zero or negative selects one per CPU. Use 1 for strictly serial file
	// reads when the live archive sits on a spinning disk or NAS share. The
	// page scan is unaffected: its disk reads are sequential at any setting.
	Jobs int
	// Progress, if non-nil, receives structured progress events as Create
	// runs. nil means fully silent. Create emits events freely and cheaply;
	// throttling for display is a rendering concern of the callback, not
	// Create's.
	Progress func(ProgressEvent)
}

// Create captures one snapshot: freeze -> scan -> pack -> index -> manifest
// (written last). See FORMAT.md.
func Create(ctx context.Context, r *Repo, app App, opts CreateOptions) (*Manifest, error) {
	if opts.MetadataSource != nil {
		return createPortable(ctx, r, app, opts)
	}
	if err := validatePackExtension(app.PackFileExtension()); err != nil {
		return nil, err
	}
	start := time.Now()
	pr := newProgressEmitter(opts.Progress)
	if opts.ZstdLevel == 0 {
		opts.ZstdLevel = pack.DefaultZstdLevel
	}
	if opts.Freezer == nil {
		opts.Freezer = NoopFreezeCoordinator{}
	}

	lock, err := r.AcquireExclusiveLock("create", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()
	if err := r.CleanStaging(); err != nil {
		return nil, err
	}

	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}
	fetch := func(id pack.BlobID) ([]byte, error) { return r.ReadBlob(known, id, nil, app.PackFileExtension()) }

	parent, err := r.LatestSnapshot()
	if err != nil {
		return nil, err
	}
	parentHash, err := loadParentHashMap(r, parent, opts.CacheDir, fetch)
	if err != nil {
		return nil, err
	}

	// The scan handle opens BEFORE the freeze, and its identity is re-proven
	// against the path after the frozen session is established: SQLite
	// resolves DBPath on its own inside OpenFrozenSession, so a path swapped
	// between the two opens would freeze one file while the scanner captures
	// another — storing a different file's bytes as page blobs while the
	// manifest records the frozen database's stats. Anchoring the handle
	// first and re-checking after fails closed on any replacement that
	// persists; only a swap-in/swap-out timed exactly around SQLite's own
	// open remains outside what path-based opens can detect.
	dbFile, err := os.Open(opts.DBPath)
	if err != nil {
		return nil, fmt.Errorf("backup: opening DB for scan: %w", err)
	}
	defer func() { _ = dbFile.Close() }()

	pr.emit(ProgressEvent{Stage: ProgressStageFreeze, Total: 1})
	session, err := OpenFrozenSessionWithOpener(ctx, opts.DBPath, opts.Freezer, opts.SQLiteOpener)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	if err := verifyScanFileIdentity(dbFile, opts.DBPath); err != nil {
		return nil, err
	}
	dbBytes := int64(session.PageCount * uint64(session.PageSize))
	pr.emit(ProgressEvent{
		Stage: ProgressStageFreeze, Done: 1, Total: 1,
		BytesDone: dbBytes, BytesTotal: dbBytes, Final: true,
	})

	view := app.FrozenView(session)
	statsRaw, err := view.Stats(ctx)
	if err != nil {
		return nil, err
	}
	info, err := view.ContentInfo(ctx)
	if err != nil {
		return nil, err
	}
	auxiliary, err := auxiliaryArtifacts(ctx, view)
	if err != nil {
		return nil, err
	}

	if parentHash != nil && parentHash.PageSize != session.PageSize {
		parentHash = nil // page size changed (e.g. VACUUM INTO); full re-capture
	}
	// parentMapUsable tracks whether the parent's page-map chain is safe to
	// materialize and merge against this scan: false both when there is no
	// parent and when the parent's page size no longer matches the live DB
	// (parentHash was just nulled above for that case). ParentID/lineage on
	// the manifest still records the true parent snapshot either way.
	parentMapUsable := parentHash != nil
	pageBytes := int64(session.PageSize)
	scan, err := ScanPages(ctx, dbFile, session.PageSize, session.PageCount, parentHash, func(done, total uint64) {
		pr.emit(ProgressEvent{
			Stage:      ProgressStageScan,
			Done:       int64(done),
			Total:      int64(total),
			BytesDone:  int64(done) * pageBytes,
			BytesTotal: int64(total) * pageBytes,
		})
	})
	if err != nil {
		return nil, err
	}

	pr.emit(ProgressEvent{
		Stage: ProgressStageScan, Done: int64(scan.PageCount), Total: int64(scan.PageCount),
		BytesDone: dbBytes, BytesTotal: dbBytes, Final: true,
	})

	appender := NewPackAppender(r, known, opts.ZstdLevel, nil, app.PackFileExtension())
	ok := false
	defer func() {
		if !ok {
			appender.Abort()
		}
	}()

	delta, err := storePageBlobs(ctx, dbFile, scan, appender, opts.Jobs, pr)
	if err != nil {
		return nil, err
	}
	manifestAuxiliary, err := captureAuxiliaryArtifacts(
		ctx, r, auxiliary, opts.ZstdLevel, appender,
	)
	if err != nil {
		return nil, err
	}
	// The page scan is done: release the frozen session's pinned read
	// transaction now so the live database can checkpoint its WAL again,
	// instead of holding the pin through attachment capture and pack sealing.
	// Everything below reads only values already copied out of the session
	// (PageSize, statsRaw, info). Close is idempotent, so the deferred close
	// above remains correct for earlier error paths.
	if err := session.Close(); err != nil {
		return nil, fmt.Errorf("backup: releasing frozen session: %w", err)
	}
	_ = dbFile.Close()

	keyframe, chainDepth, err := decideKeyframe(r, parent, parentHash, known)
	if err != nil {
		return nil, err
	}
	mapObj, err := buildPageMapObject(r, parent, parentMapUsable, delta, keyframe, fetch)
	if err != nil {
		return nil, err
	}
	mapBlob, _, err := appender.Add(mapObj)
	if err != nil {
		return nil, err
	}
	var hashObj []byte
	if keyframe {
		hashObj = EncodeHashKeyframe(&PageHashMap{PageSize: scan.PageSize, PageCount: scan.PageCount, Hashes: scan.Hashes})
	} else {
		hashObj = EncodeHashDelta(BuildHashDelta(scan))
	}
	hashBlob, _, err := appender.Add(hashObj)
	if err != nil {
		return nil, err
	}

	capture, lists, extras, err := captureSnapshotFiles(
		ctx, r, app, opts, parent, known, info, appender, pr)
	if err != nil {
		return nil, err
	}

	sealed, err := sealSnapshotCapture(ctx, r, appender, pr)
	if err != nil {
		return nil, err
	}
	ok = true
	if err := r.SetPageSize(int(session.PageSize)); err != nil {
		return nil, err
	}

	createdAt, err := nextCreatedAt(time.Now(), parent)
	if err != nil {
		return nil, err
	}

	// A snapshot whose attachment population records non-canonical storage
	// paths needs a path-aware restore: version-1 readers placed every blob
	// at "<aa>/<hash>" and would materialize a database pointing at missing
	// files, so such manifests require reader version 2 and old readers
	// refuse them explicitly instead of restoring a broken tree.
	manifestVersion := FormatVersion
	manifestMinReader := MinReaderVersion
	if info.NonCanonicalPaths {
		manifestVersion = dbPathManifestVersion
		manifestMinReader = dbPathManifestVersion
	}
	if len(manifestAuxiliary) > 0 {
		manifestVersion = max(manifestVersion, auxiliaryManifestVersion)
		manifestMinReader = max(manifestMinReader, auxiliaryManifestVersion)
	}
	if len(capture.Recipes) > 0 {
		manifestVersion = max(manifestVersion, chunkedObjectManifestVersion)
		manifestMinReader = max(manifestMinReader, chunkedObjectManifestVersion)
	}
	if len(extras.Recipes) > 0 {
		manifestVersion = max(manifestVersion, chunkedExtrasManifestVersion)
		manifestMinReader = max(manifestMinReader, chunkedExtrasManifestVersion)
	}
	m := &Manifest{
		FormatVersion:    manifestVersion,
		MinReaderVersion: manifestMinReader,
		AppVersion:       app.Version(),
		CreatedAt:        createdAt.Format(time.RFC3339),
		Options: ManifestOptions{
			IncludeConfig: opts.IncludeConfig,
			IncludeTokens: opts.IncludeTokens,
			ZstdLevel:     opts.ZstdLevel,
			Tag:           opts.Tag,
		},
		DB: ManifestDB{
			Engine:        "sqlite",
			PageSize:      scan.PageSize,
			PageCount:     scan.PageCount,
			PageMap:       mapBlob.String(),
			PageHashMap:   hashBlob.String(),
			MapChainDepth: chainDepth,
		},
		Auxiliary: manifestAuxiliary,
		Attachments: ManifestAttachments{
			Layout:    []string{"loose"},
			Rows:      info.Rows,
			Blobs:     capture.Blobs,
			BlobBytes: capture.BlobBytes,
			Recipes:   capture.Recipes,
			Lists:     lists,
		},
		Excluded:        app.ExcludedPaths(),
		Stats:           statsRaw,
		NewPacks:        sealed.newPacks,
		NewIndex:        sealed.newIndex,
		DurationSeconds: time.Since(start).Seconds(),
		BytesAdded:      sealed.bytesAdded,
	}
	if parent != nil {
		m.ParentID = parent.SnapshotID
	}
	m.Extras = extras
	// Final gate: packs and index are durable at this point (harmless
	// unreferenced data if abandoned, like a crash here), but the manifest is
	// what makes the snapshot exist. Never publish one after cancellation.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := r.WriteManifest(m)
	if err != nil {
		return nil, err
	}
	m.SnapshotID = id

	// The local cache is disposable: a save failure must not fail the backup.
	fullHash := &PageHashMap{PageSize: scan.PageSize, PageCount: scan.PageCount, Hashes: scan.Hashes}
	_ = SaveHashMapCache(opts.CacheDir, r.Config().RepoID, id, fullHash)
	return m, nil
}

// verifyScanFileIdentity requires that path still resolves to the already-open
// scan handle's file, tying the file the page scan will read to the file the
// frozen SQLite session opened by the same path moments earlier. See the
// comment at the os.Open in Create for the race this closes.
func verifyScanFileIdentity(f *os.File, path string) error {
	held, err := f.Stat()
	if err != nil {
		return fmt.Errorf("backup: checking scan handle: %w", err)
	}
	byPath, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("backup: re-checking database path: %w", err)
	}
	if !os.SameFile(held, byPath) {
		return fmt.Errorf(
			"backup: database at %s was replaced while opening the freeze; retry the backup", path)
	}
	return nil
}

// nextCreatedAt returns the timestamp to record as this snapshot's
// CreatedAt. Snapshot IDs embed CreatedAt truncated to 1-second resolution
// (FORMAT.md), and ListSnapshots/LatestSnapshot rely on lexicographic ID
// order matching chronological order. Create holds the repo's exclusive
// lock for its entire run, so it can safely enforce that invariant here: if
// the parent's timestamp (truncated to seconds) is not strictly before
// now's, the new timestamp is bumped to one second past the parent's. This
// guarantees every new snapshot's ID sorts after its parent's even when two
// Create calls land in the same wall-clock second.
func nextCreatedAt(now time.Time, parent *Manifest) (time.Time, error) {
	now = now.UTC()
	if parent == nil {
		return now, nil
	}
	parentCreatedAt, err := time.Parse(time.RFC3339, parent.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("backup: parent snapshot %s created_at %q: %w", parent.SnapshotID, parent.CreatedAt, err)
	}
	parentSecond := parentCreatedAt.UTC().Truncate(time.Second)
	if !now.Truncate(time.Second).After(parentSecond) {
		return parentSecond.Add(time.Second), nil
	}
	return now, nil
}

// loadParentHashMap returns the parent snapshot's page-hash map, used as the
// baseline the dirty-page scan compares live pages against. It prefers the
// local cache only when the cache can be fully authenticated against the
// parent manifest (parentCacheAuthentic); otherwise it materializes the map
// from the repository's page-hash-map chain, which is authenticated end to end
// by blob content addresses.
//
// The rebuild is cheap relative to the work Create already does: the dirty-page
// scan reads and hashes the entire live database, so reading the parent chain
// (a keyframe map is PageCount×pageHashSize bytes plus a bounded number of
// deltas) is modest by comparison. Rebuilding also re-anchors authentication
// on every snapshot instead of only keyframe-parented ones. The cache write
// path stays as is: the cache still serves the common keyframe-parent case
// (chain starts plus roughly one snapshot in keyframeChainMax) and stays warm
// for whenever the parent is itself a keyframe.
func loadParentHashMap(r *Repo, parent *Manifest, cacheDir string, fetch func(pack.BlobID) ([]byte, error)) (*PageHashMap, error) {
	if parent == nil || parent.Metadata != nil {
		return nil, nil
	}
	if cacheDir != "" {
		snapID, cached, err := LoadHashMapCache(cacheDir, r.Config().RepoID)
		if err == nil && cached != nil && snapID == parent.SnapshotID &&
			parentCacheAuthentic(cached, parent) {
			return cached, nil
		}
	}
	chain, err := r.HashMapChain(parent)
	if err != nil {
		return nil, err
	}
	return MaterializeHashMap(fetch, chain)
}

// parentCacheAuthentic reports whether a cached parent page-hash map can be
// trusted as the parent snapshot's map without rematerializing it from the
// repository. A snapshot-ID match alone is not enough: the cache is a local,
// disposable sidecar that a bug, a stale write, or on-disk corruption could
// leave self-consistent (its own SHA-256 trailer intact, already verified in
// LoadHashMapCache) yet describing the wrong page hashes. Trusting such a map
// would make the dirty-page scan compare live pages against wrong parent
// hashes and silently drop changed pages from the delta — corrupting the
// snapshot chain while Create reports success.
//
// Only a keyframe parent (chain depth 0) can be authenticated: its manifest
// PageHashMap is the content address of the keyframe object — the blob ID of
// EncodeHashKeyframe(full map) — so recomputing that blob ID from the cache and
// requiring equality authenticates every cached hash. A delta-chained parent
// carries no full-map digest in the frozen wire format, so its cache cannot be
// authenticated end to end and is never trusted; loadParentHashMap rebuilds
// such parents from the repository chain instead. The geometry cross-check
// (page size and count) stays as a cheap early reject on the authenticated
// path.
func parentCacheAuthentic(cached *PageHashMap, parent *Manifest) bool {
	if parent.DB.MapChainDepth != 0 {
		return false
	}
	if cached.PageSize != parent.DB.PageSize || cached.PageCount != parent.DB.PageCount {
		return false
	}
	return pack.ComputeBlobID(EncodeHashKeyframe(cached)).String() == parent.DB.PageHashMap
}

// parentUnionShrank reports whether any hash the parent's attachment lists
// enumerate is missing from the current frozen ref set. When true, the
// append-only list inheritance would leave the manifest's list union a strict
// superset of the current population, so Create writes a fresh full list
// instead.
func parentUnionShrank(parentSeen map[string]bool, refs []ContentRef) bool {
	if len(parentSeen) == 0 {
		return false
	}
	current := make(map[string]bool, len(refs))
	for _, ref := range refs {
		current[ref.Hash] = true
	}
	for hash := range parentSeen {
		if !current[hash] {
			return true
		}
	}
	return false
}

// pageBlobResult is one worker's read+hash+compress output for plans[index].
type pageBlobResult struct {
	index      int
	id         pack.BlobID
	frame      []byte
	rawLen     uint64
	compressed bool
	known      bool
	err        error
}

// buildPageBlob reads, hashes, and (for blobs the repository does not
// already hold) trial-compresses one planned dirty-page blob. Runs on a
// worker; dbFile reads use ReadAt, which is concurrent-safe.
func buildPageBlob(
	dbFile *os.File, pageSize uint32, plan BlobPlan, index int,
	preKnown map[pack.BlobID]struct{}, level int,
) pageBlobResult {
	content, err := BuildBlobContent(dbFile, pageSize, plan)
	if err != nil {
		return pageBlobResult{index: index, err: err}
	}
	res := pageBlobResult{index: index, id: pack.ComputeBlobID(content), rawLen: uint64(len(content))}
	if _, ok := preKnown[res.id]; ok {
		res.known = true
		return res
	}
	res.frame, res.compressed = pack.EncodeFrame(content, level)
	return res
}

// storePageBlobs stores every dirty-page blob and returns the page-map delta
// describing the new blobs. Reading and compression fan out to jobs workers
// (zero or negative: one per CPU; one: strictly serial reads, for archives
// on spinning disks or NAS shares); a single in-order collector feeds the
// appender, so pack contents and the delta are identical to a serial run.
// Progress is reported per plan under ProgressStagePack — on a first backup
// this phase, not the scan, is most of the wall clock.
func storePageBlobs(
	ctx context.Context,
	dbFile *os.File, scan *ScanResult, appender *PackAppender, jobs int, pr *progressEmitter,
) (*PageMap, error) {
	delta := &PageMap{PageSize: scan.PageSize, PageCount: scan.PageCount}
	plans := PlanBlobs(scan.Dirty)
	if len(plans) == 0 {
		return delta, nil
	}
	var totalPages int64
	for i := range plans {
		totalPages += int64(plans[i].Pages())
	}
	pageBytes := int64(scan.PageSize)
	pr.emit(ProgressEvent{Stage: ProgressStagePack, Total: totalPages, BytesTotal: totalPages * pageBytes})

	workers := jobs
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, len(plans))
	preKnown := appender.knownSnapshot()
	level := appender.zstdLevel

	// inflight bounds dispatched-but-unrecorded plans; each holds at most
	// one blob (4 MiB max), so memory stays small without a byte gate.
	inflight := workers + 2
	stop := make(chan struct{})
	work := make(chan int)
	results := make(chan pageBlobResult, inflight)
	tokens := make(chan struct{}, inflight)

	go func() {
		defer close(work)
		for i := range plans {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case tokens <- struct{}{}:
			}
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case work <- i:
			}
		}
	}()
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range work {
				results <- buildPageBlob(dbFile, scan.PageSize, plans[i], i, preKnown, level)
			}
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	blobIdx := map[pack.BlobID]uint32{}
	pending := map[int]pageBlobResult{}
	next := 0
	var donePages int64
	var firstErr error
	for res := range results {
		if firstErr != nil {
			continue // draining after failure
		}
		if err := ctx.Err(); err != nil {
			firstErr = err
			close(stop)
			continue
		}
		pending[res.index] = res
		for firstErr == nil {
			c, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			<-tokens
			if c.err != nil {
				firstErr = c.err
			} else {
				firstErr = recordPageBlob(c, plans[c.index], scan.PageSize, appender, delta, blobIdx)
			}
			if firstErr != nil {
				close(stop)
				continue
			}
			donePages += int64(plans[c.index].Pages())
			pr.emit(ProgressEvent{
				Stage: ProgressStagePack, Done: donePages, Total: totalPages,
				BytesDone: donePages * pageBytes, BytesTotal: totalPages * pageBytes,
			})
		}
	}
	if firstErr == nil {
		// The dispatcher exits silently on ctx.Done; without this check an
		// early cancellation could return a partial delta as success.
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(delta.Runs, func(i, j int) bool { return delta.Runs[i].StartPage < delta.Runs[j].StartPage })
	pr.emit(ProgressEvent{
		Stage: ProgressStagePack, Done: totalPages, Total: totalPages,
		BytesDone: totalPages * pageBytes, BytesTotal: totalPages * pageBytes, Final: true,
	})
	return delta, nil
}

// recordPageBlob applies one worker result in plan order: append the frame
// (unless the blob is already stored — AddEncoded also dedupes repeats
// within this run) and record the plan's runs against the blob's index.
func recordPageBlob(
	c pageBlobResult, plan BlobPlan, pageSize uint32,
	appender *PackAppender, delta *PageMap, blobIdx map[pack.BlobID]uint32,
) error {
	if !c.known {
		if _, err := appender.AddEncoded(c.id, c.frame, c.rawLen, c.compressed); err != nil {
			return err
		}
	}
	idx, seen := blobIdx[c.id]
	if !seen {
		idx = uint32(len(delta.Blobs))
		delta.Blobs = append(delta.Blobs, c.id)
		blobIdx[c.id] = idx
	}
	delta.Runs = append(delta.Runs, RunsForPlan(plan, idx, pageSize)...)
	return nil
}

// decideKeyframe applies the keyframe cadence (FORMAT.md, Page-Map Objects): keyframe when
// there is no usable parent, the chain would reach keyframeChainMax, or the
// chain's stored delta bytes exceed the keyframe's stored bytes.
func decideKeyframe(r *Repo, parent *Manifest, parentHash *PageHashMap, known map[pack.BlobID]IndexEntry) (bool, int, error) {
	if parent == nil || parentHash == nil {
		return true, 0, nil
	}
	depth := parent.DB.MapChainDepth + 1
	if depth >= keyframeChainMax {
		return true, 0, nil
	}
	chain, err := r.PageMapChain(parent)
	if err != nil {
		return false, 0, err
	}
	var deltaBytes, keyframeBytes uint64
	for i, id := range chain {
		e, ok := known[id]
		if !ok {
			return false, 0, fmt.Errorf("backup: page-map chain blob %s missing from indexes", id)
		}
		if i == len(chain)-1 {
			keyframeBytes = e.StoredLen
		} else {
			deltaBytes += e.StoredLen
		}
	}
	if deltaBytes > keyframeBytes {
		return true, 0, nil
	}
	return false, depth, nil
}

// buildPageMapObject encodes this snapshot's page-map object: the delta
// itself, or a merged keyframe when the cadence calls for one. parentMapUsable
// is false when there is no parent, or when the parent's page-map chain was
// built at a page size that no longer matches this scan (e.g. VACUUM changed
// page_size); in that case the parent chain is never walked and the delta —
// which already covers every page, since a page-size change forces a full
// rescan — becomes the keyframe as-is.
func buildPageMapObject(r *Repo, parent *Manifest, parentMapUsable bool, delta *PageMap, keyframe bool, fetch func(pack.BlobID) ([]byte, error)) ([]byte, error) {
	if !keyframe {
		return EncodePageMap(delta, true), nil
	}
	full := delta
	if parentMapUsable && parent != nil {
		chain, err := r.PageMapChain(parent)
		if err != nil {
			return nil, fmt.Errorf("backup: loading parent page-map chain for keyframe: %w", err)
		}
		parentMap, err := MaterializePageMap(fetch, chain)
		if err != nil {
			return nil, err
		}
		full, err = ApplyPageMapDelta(parentMap, delta)
		if err != nil {
			return nil, err
		}
	}
	if err := full.CheckCoverage(); err != nil {
		return nil, fmt.Errorf("backup: keyframe page map incomplete: %w", err)
	}
	return EncodePageMap(full, false), nil
}
