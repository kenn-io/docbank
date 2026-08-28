package vectorworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
	"go.kenn.io/kit/packstore"
)

var ErrVectorIndexCandidateInvalid = errors.New("vector index candidate is invalid")

var errVectorIndexSourceAbsent = errors.New("vector index source is absent")

type UnavailableVectorSet = store.VectorIndexUnavailableSet
type UnavailableVectorCoverage = store.VectorIndexUnavailableCoverage

type IndexRestoreReport struct {
	Rebuilt     []store.VectorIndexGenerationRecord
	Unavailable []UnavailableVectorCoverage
}

type Catalog interface {
	CaptureVectorIndexSource(ctx context.Context, vectorSpaceID string) (store.VectorIndexSource, error)
	ListVectorIndexSpaces(ctx context.Context) ([]string, error)
	ClaimVectorIndexBuild(ctx context.Context, vectorSpaceID, sourceChecksum, owner string, at time.Time, lease time.Duration) (store.VectorIndexBuildClaim, bool, error)
	StageVectorIndexGeneration(ctx context.Context, claim store.VectorIndexBuildClaim, record store.VectorIndexGenerationRecord, at time.Time) error
	LoadVectorIndexGeneration(ctx context.Context, generationID string) (store.VectorIndexGenerationRecord, error)
	PublishVectorIndexGeneration(ctx context.Context, claim store.VectorIndexBuildClaim, generationID string, at time.Time) error
	ActiveVectorIndexGeneration(ctx context.Context, vectorSpaceID string) (store.VectorIndexGenerationRecord, error)
	AcquireVectorIndexGeneration(ctx context.Context, vectorSpaceID, owner string, at time.Time, lease time.Duration) (store.VectorIndexReaderLease, error)
	ReleaseVectorIndexGeneration(ctx context.Context, leaseID string, fencingToken int64, at time.Time) error
	ReclaimVectorIndexGenerations(ctx context.Context, at time.Time) (int, error)
	ReplaceVectorIndexUnavailableCoverage(ctx context.Context, coverage []store.VectorIndexUnavailableCoverage) error
}

type BlobReader interface {
	OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error)
}

type OperationGate interface {
	PreserveContext(ctx context.Context, fn func() error) error
}

type IndexWorkerConfig struct {
	Catalog       Catalog
	Blobs         BlobReader
	Gate          OperationGate
	Owner         string
	BuildLease    time.Duration
	ReaderLease   time.Duration
	IdleDelay     time.Duration
	MaxRetryDelay time.Duration
	BuildOptions  vectorindex.Options
	Clock         func() time.Time
	Wait          func(context.Context, time.Duration) error
}

type indexBuildCall struct {
	done   chan struct{}
	record store.VectorIndexGenerationRecord
	err    error
}

type IndexWorker struct {
	catalog       Catalog
	blobs         BlobReader
	gate          OperationGate
	owner         string
	buildLease    time.Duration
	readerLease   time.Duration
	idleDelay     time.Duration
	maxRetryDelay time.Duration
	buildOptions  vectorindex.Options
	clock         func() time.Time
	wait          func(context.Context, time.Duration) error

	mu       sync.Mutex
	inflight map[string]*indexBuildCall
}

func NewIndexWorker(config IndexWorkerConfig) (*IndexWorker, error) {
	if config.Catalog == nil {
		return nil, errors.New("vector index catalog is required")
	}
	if config.Blobs == nil || config.Gate == nil {
		return nil, errors.New("vector index managed blob reader and operation gate are required")
	}
	if config.Owner == "" {
		return nil, errors.New("vector index worker owner is required")
	}
	if config.BuildLease <= 0 || config.ReaderLease <= 0 || config.IdleDelay <= 0 {
		return nil, errors.New("vector index worker durations must be positive")
	}
	if config.MaxRetryDelay == 0 {
		config.MaxRetryDelay = 30 * config.IdleDelay
	}
	if config.MaxRetryDelay < config.IdleDelay {
		return nil, errors.New("vector index maximum retry delay must not be shorter than idle delay")
	}
	bounds, err := vectorindex.EffectiveOptions(config.BuildOptions)
	if err != nil {
		return nil, err
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Wait == nil {
		config.Wait = waitForVectorRetry
	}
	return &IndexWorker{catalog: config.Catalog, owner: config.Owner,
		blobs: config.Blobs, gate: config.Gate,
		buildLease: config.BuildLease, readerLease: config.ReaderLease,
		idleDelay: config.IdleDelay, maxRetryDelay: config.MaxRetryDelay,
		buildOptions: bounds, clock: config.Clock, wait: config.Wait,
		inflight: make(map[string]*indexBuildCall)}, nil
}

func waitForVectorRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (worker *IndexWorker) Rebuild(ctx context.Context, vectorSpaceID string) (store.VectorIndexGenerationRecord, error) {
	worker.mu.Lock()
	if call := worker.inflight[vectorSpaceID]; call != nil {
		worker.mu.Unlock()
		select {
		case <-ctx.Done():
			return store.VectorIndexGenerationRecord{}, ctx.Err()
		case <-call.done:
			return cloneVectorIndexRecord(call.record), call.err
		}
	}
	call := &indexBuildCall{done: make(chan struct{})}
	worker.inflight[vectorSpaceID] = call
	worker.mu.Unlock()

	call.err = worker.gate.PreserveContext(ctx, func() error {
		call.record, call.err = worker.rebuild(ctx, vectorSpaceID)
		return call.err
	})
	worker.mu.Lock()
	delete(worker.inflight, vectorSpaceID)
	close(call.done)
	worker.mu.Unlock()
	return cloneVectorIndexRecord(call.record), call.err
}

func cloneVectorIndexRecord(record store.VectorIndexGenerationRecord) store.VectorIndexGenerationRecord {
	record.Bytes = bytes.Clone(record.Bytes)
	return record
}

type unavailableVectorSourceError struct{ coverage UnavailableVectorCoverage }

func (err *unavailableVectorSourceError) Error() string {
	return fmt.Sprintf("vector space %s has %d unavailable canonical vector sets",
		err.coverage.VectorSpaceID, len(err.coverage.Missing))
}

func (worker *IndexWorker) rebuild(ctx context.Context, vectorSpaceID string) (store.VectorIndexGenerationRecord, error) {
	source, err := worker.catalog.CaptureVectorIndexSource(ctx, vectorSpaceID)
	if errors.Is(err, store.ErrNotFound) {
		return store.VectorIndexGenerationRecord{}, errors.Join(errVectorIndexSourceAbsent, err)
	}
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	setIDs, bySetID, err := preflightVectorIndexSource(source, worker.buildOptions)
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	manifest, err := vectorindex.NewManifest(setIDs)
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if active, activeErr := worker.catalog.ActiveVectorIndexGeneration(ctx, vectorSpaceID); activeErr == nil &&
		active.SourceManifestChecksum == source.ManifestChecksum {
		if validateErr := validateStoredVectorIndex(active, source, nil); validateErr == nil {
			return active, nil
		}
	}
	built, firstQuery, coverage, err := worker.loadVectorSets(ctx, source, manifest, bySetID)
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if len(coverage.Missing) != 0 {
		return store.VectorIndexGenerationRecord{}, &unavailableVectorSourceError{coverage: coverage}
	}
	claim, claimed, err := worker.catalog.ClaimVectorIndexBuild(ctx, vectorSpaceID,
		source.ManifestChecksum, worker.owner, worker.clock().UTC(), worker.buildLease)
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if !claimed {
		return store.VectorIndexGenerationRecord{}, store.ErrVectorIndexBuildInProgress
	}
	metadata := built.Metadata()
	encoded := built.Bytes()
	record := store.VectorIndexGenerationRecord{ID: indexGenerationID(encoded),
		VectorSpaceID: vectorSpaceID, SourceManifestChecksum: source.ManifestChecksum,
		IndexManifestChecksum: metadata.Manifest.Checksum, Bytes: encoded,
		RowCount: metadata.RowCount, BuiltAt: indexWorkerTimestamp(worker.clock().UTC())}
	if err := worker.catalog.StageVectorIndexGeneration(ctx, claim, record, worker.clock().UTC()); err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	stored, err := worker.catalog.LoadVectorIndexGeneration(ctx, record.ID)
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if err := validateStoredVectorIndex(stored, source, firstQuery); err != nil {
		return store.VectorIndexGenerationRecord{}, errors.Join(ErrVectorIndexCandidateInvalid, err)
	}
	if err := worker.catalog.PublishVectorIndexGeneration(ctx, claim, record.ID, worker.clock().UTC()); err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if _, err := worker.catalog.ReclaimVectorIndexGenerations(ctx, worker.clock().UTC()); err != nil {
		return stored, fmt.Errorf("reclaiming obsolete vector index generations: %w", err)
	}
	return stored, nil
}

func preflightVectorIndexSource(source store.VectorIndexSource, bounds vectorindex.Options) (
	[]string, map[string]store.VectorIndexMember, error,
) {
	if len(source.Members) == 0 || len(source.Members) > bounds.MaxRows {
		return nil, nil, errors.New("vector index source membership exceeds build bounds")
	}
	bySetID := make(map[string]store.VectorIndexMember, len(source.Members))
	var payloadBytes int64
	for _, member := range source.Members {
		if member.PayloadSize < 1 || member.PayloadSize > 64<<20 {
			return nil, nil, errors.New("vector index source payload exceeds build bounds")
		}
		if _, err := packstore.ParseHash(member.PayloadBlobHash); err != nil {
			return nil, nil, fmt.Errorf("vector index source payload hash is invalid: %w", err)
		}
		prior, duplicate := bySetID[member.VectorSetID]
		if duplicate {
			if prior.PayloadBlobHash != member.PayloadBlobHash || prior.PayloadSize != member.PayloadSize {
				return nil, nil, errors.New("vector index source repeats a set with inconsistent physical authority")
			}
			continue
		}
		if member.PayloadSize > bounds.MaxBytes-payloadBytes {
			return nil, nil, errors.New("vector index source aggregate payload exceeds build bounds")
		}
		payloadBytes += member.PayloadSize
		bySetID[member.VectorSetID] = member
	}
	if len(bySetID) > bounds.MaxRows {
		return nil, nil, errors.New("vector index source set count exceeds build bounds")
	}
	setIDs := make([]string, 0, len(bySetID))
	for setID := range bySetID {
		setIDs = append(setIDs, setID)
	}
	slices.Sort(setIDs)
	return setIDs, bySetID, nil
}

func (worker *IndexWorker) loadVectorSets(ctx context.Context, source store.VectorIndexSource,
	manifest vectorindex.Manifest, bySetID map[string]store.VectorIndexMember,
) (vectorindex.Generation, []float32, UnavailableVectorCoverage, error) {
	coverage := UnavailableVectorCoverage{VectorSpaceID: source.VectorSpaceID,
		SourceManifestChecksum: source.ManifestChecksum, ExternalReembeddingRequired: true}
	builder, err := vectorindex.NewBuilder(manifest, worker.buildOptions)
	if err != nil {
		return vectorindex.Generation{}, nil, coverage, err
	}
	var firstQuery []float32
	for _, setID := range manifest.SetIDs {
		member := bySetID[setID]
		payload, err := worker.readVectorSet(ctx, member)
		if errors.Is(err, store.ErrVectorSetUnavailable) {
			for _, logical := range source.Members {
				if logical.VectorSetID == setID {
					coverage.Missing = append(coverage.Missing, UnavailableVectorSet{
						EmbeddingSetID: logical.EmbeddingSetID, VectorSetID: setID,
						PayloadBlobHash: logical.PayloadBlobHash})
				}
			}
			continue
		}
		if err != nil {
			return vectorindex.Generation{}, nil, coverage, err
		}
		remainingRows := worker.buildOptions.MaxRows - builder.RowCount()
		set, err := document.DecodeVectorSetV1(payload, document.VectorBounds{
			MaxRows: remainingRows, MaxDimension: worker.buildOptions.MaxDimension, MaxBytes: len(payload)})
		if err != nil {
			return vectorindex.Generation{}, nil, coverage,
				fmt.Errorf("decoding canonical vector set %s: %w", setID, err)
		}
		canonical, checksum, err := document.EncodeVectorSetV1(set)
		if err != nil || checksum != setID || !bytes.Equal(canonical, payload) ||
			set.VectorSpaceFingerprint != source.VectorSpaceID {
			return vectorindex.Generation{}, nil, coverage,
				errors.New("vector index source payload is not exact canonical authority")
		}
		if err := builder.Add(setID, set); err != nil {
			return vectorindex.Generation{}, nil, coverage, err
		}
		if firstQuery == nil {
			firstQuery = append([]float32(nil), set.Vectors[0]...)
		}
	}
	if len(coverage.Missing) != 0 {
		return vectorindex.Generation{}, nil, coverage, nil
	}
	built, err := builder.Build()
	return built, firstQuery, coverage, err
}

func (worker *IndexWorker) readVectorSet(ctx context.Context,
	member store.VectorIndexMember,
) (_ []byte, retErr error) {
	stream, size, err := worker.blobs.OpenStreamContext(ctx, member.PayloadBlobHash)
	if err != nil {
		if isUnavailableVectorBlob(err) {
			return nil, fmt.Errorf("%w: vector set %s blob %s: %w", store.ErrVectorSetUnavailable,
				member.VectorSetID, member.PayloadBlobHash, err)
		}
		return nil, fmt.Errorf("opening vector set %s blob %s: %w",
			member.VectorSetID, member.PayloadBlobHash, err)
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	if size != member.PayloadSize || size < 1 || size > 64<<20 {
		return nil, fmt.Errorf("vector set %s payload size does not match catalog: %w",
			member.VectorSetID, packstore.ErrPhysicalCorrupt)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(stream, payload); err != nil {
		if isUnavailableVectorBlob(err) {
			return nil, fmt.Errorf("%w: vector set %s blob %s: %w", store.ErrVectorSetUnavailable,
				member.VectorSetID, member.PayloadBlobHash, err)
		}
		return nil, fmt.Errorf("reading vector set %s blob %s: %w",
			member.VectorSetID, member.PayloadBlobHash, err)
	}
	if err := stream.Verify(); err != nil {
		return nil, fmt.Errorf("verifying vector set %s blob %s: %w",
			member.VectorSetID, member.PayloadBlobHash, err)
	}
	return payload, nil
}

func isUnavailableVectorBlob(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, packstore.ErrStoreUnavailable) ||
		errors.Is(err, packstore.ErrStoreFenced) || errors.Is(err, packstore.ErrPhysicalMissing) ||
		errors.Is(err, packstore.ErrPhysicalAuthorityMissing)
}

func validateStoredVectorIndex(record store.VectorIndexGenerationRecord, source store.VectorIndexSource,
	smokeQuery []float32,
) error {
	if indexGenerationID(record.Bytes) != record.ID || record.VectorSpaceID != source.VectorSpaceID ||
		record.SourceManifestChecksum != source.ManifestChecksum {
		return errors.New("vector index candidate identity does not match source authority")
	}
	generation, err := vectorindex.OpenGeneration(bytes.NewReader(record.Bytes), int64(len(record.Bytes)))
	if err != nil {
		return err
	}
	metadata := generation.Metadata()
	if metadata.VectorSpaceID != record.VectorSpaceID || metadata.Manifest.Checksum != record.IndexManifestChecksum ||
		metadata.RowCount != record.RowCount {
		return errors.New("vector index candidate metadata does not match local catalog")
	}
	if smokeQuery != nil {
		neighbors, err := generation.Search(smokeQuery, 1, metadata.RowCount)
		if err != nil || len(neighbors) != 1 {
			return errors.New("vector index candidate smoke search failed")
		}
	}
	return nil
}

func (worker *IndexWorker) Restore(ctx context.Context) (IndexRestoreReport, error) {
	spaces, err := worker.catalog.ListVectorIndexSpaces(ctx)
	if err != nil {
		return IndexRestoreReport{}, err
	}
	slices.Sort(spaces)
	report := IndexRestoreReport{}
	for _, space := range spaces {
		record, err := worker.Rebuild(ctx, space)
		if errors.Is(err, errVectorIndexSourceAbsent) {
			continue
		}
		if unavailable, ok := errors.AsType[*unavailableVectorSourceError](err); ok {
			report.Unavailable = append(report.Unavailable, unavailable.coverage)
			continue
		}
		if err != nil {
			return report, err
		}
		report.Rebuilt = append(report.Rebuilt, record)
	}
	if err := worker.catalog.ReplaceVectorIndexUnavailableCoverage(ctx, report.Unavailable); err != nil {
		return report, err
	}
	return report, nil
}

type IndexLease struct {
	catalog    Catalog
	lease      store.VectorIndexReaderLease
	opened     *vectorindex.Generation
	clock      func() time.Time
	once       sync.Once
	releaseErr error
}

func (worker *IndexWorker) Acquire(ctx context.Context, vectorSpaceID, owner string) (*IndexLease, error) {
	lease, err := worker.catalog.AcquireVectorIndexGeneration(ctx, vectorSpaceID, owner,
		worker.clock().UTC(), worker.readerLease)
	if err != nil {
		return nil, err
	}
	opened, err := vectorindex.OpenGeneration(bytes.NewReader(lease.Generation.Bytes), int64(len(lease.Generation.Bytes)))
	if err != nil {
		_ = worker.catalog.ReleaseVectorIndexGeneration(context.WithoutCancel(ctx), lease.ID,
			lease.FencingToken, worker.clock().UTC())
		return nil, errors.Join(ErrVectorIndexCandidateInvalid, err)
	}
	return &IndexLease{catalog: worker.catalog, lease: lease, opened: opened, clock: worker.clock}, nil
}

func (lease *IndexLease) Search(query []float32, k int) ([]vectorindex.Neighbor, error) {
	if lease == nil || lease.opened == nil {
		return nil, errors.New("vector index reader lease is not open")
	}
	return lease.opened.Search(query, k, lease.opened.Metadata().RowCount)
}

func (lease *IndexLease) Release(ctx context.Context) error {
	if lease == nil {
		return nil
	}
	lease.once.Do(func() {
		lease.releaseErr = lease.catalog.ReleaseVectorIndexGeneration(ctx, lease.lease.ID,
			lease.lease.FencingToken, lease.clock().UTC())
		if lease.releaseErr == nil {
			_, lease.releaseErr = lease.catalog.ReclaimVectorIndexGenerations(ctx, lease.clock().UTC())
		}
	})
	return lease.releaseErr
}

func (worker *IndexWorker) Run(ctx context.Context) error {
	delay := worker.idleDelay
	running := true
	for running {
		contention, err := worker.runPass(ctx)
		if err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				break
			}
			return err
		}
		if contention {
			delay = nextVectorRetryDelay(delay, worker.maxRetryDelay)
		} else {
			delay = worker.idleDelay
		}
		if err := worker.wait(ctx, delay); err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				running = false
				continue
			}
			return fmt.Errorf("waiting for vector index retry: %w", err)
		}
	}
	return nil
}

func nextVectorRetryDelay(delay, maximum time.Duration) time.Duration {
	if delay >= maximum || delay > maximum-delay {
		return maximum
	}
	return delay * 2
}

func (worker *IndexWorker) runPass(ctx context.Context) (bool, error) {
	spaces, err := worker.catalog.ListVectorIndexSpaces(ctx)
	if err != nil {
		return false, fmt.Errorf("listing vector index spaces: %w", err)
	}
	slices.Sort(spaces)
	coverage := make([]UnavailableVectorCoverage, 0)
	contention := false
	var permanent error
	for _, space := range spaces {
		_, err := worker.Rebuild(ctx, space)
		var unavailable *unavailableVectorSourceError
		switch {
		case err == nil, errors.Is(err, errVectorIndexSourceAbsent):
		case isExpectedVectorContention(err):
			contention = true
		case errors.As(err, &unavailable):
			coverage = append(coverage, unavailable.coverage)
		default:
			permanent = errors.Join(permanent, fmt.Errorf("rebuilding vector index space %s: %w", space, err))
		}
	}
	if !contention && permanent == nil {
		if err := worker.catalog.ReplaceVectorIndexUnavailableCoverage(ctx, coverage); err != nil {
			permanent = fmt.Errorf("recording unavailable vector index coverage: %w", err)
		}
	}
	return contention, permanent
}

func isExpectedVectorContention(err error) bool {
	for _, expected := range []error{
		store.ErrVectorIndexBuildInProgress,
		store.ErrVectorIndexBuildFenced,
		store.ErrVectorIndexSourceStale,
	} {
		if errors.Is(err, expected) {
			return true
		}
	}
	return false
}

func indexGenerationID(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func indexWorkerTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
