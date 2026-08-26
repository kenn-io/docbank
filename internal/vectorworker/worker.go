package vectorworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

var ErrVectorIndexCandidateInvalid = errors.New("vector index candidate is invalid")

type UnavailableVectorSet = store.VectorIndexUnavailableSet
type UnavailableVectorCoverage = store.VectorIndexUnavailableCoverage

type IndexRestoreReport struct {
	Rebuilt     []store.VectorIndexGenerationRecord
	Unavailable []UnavailableVectorCoverage
}

type Catalog interface {
	CaptureVectorIndexSource(ctx context.Context, vectorSpaceID string) (store.VectorIndexSource, error)
	ListVectorIndexSpaces(ctx context.Context) ([]string, error)
	ReadVectorIndexVectorSet(ctx context.Context, member store.VectorIndexMember) ([]byte, error)
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

type IndexWorkerConfig struct {
	Catalog     Catalog
	Owner       string
	BuildLease  time.Duration
	ReaderLease time.Duration
	IdleDelay   time.Duration
	Clock       func() time.Time
}

type indexBuildCall struct {
	done   chan struct{}
	record store.VectorIndexGenerationRecord
	err    error
}

type IndexWorker struct {
	catalog     Catalog
	owner       string
	buildLease  time.Duration
	readerLease time.Duration
	idleDelay   time.Duration
	clock       func() time.Time

	mu       sync.Mutex
	inflight map[string]*indexBuildCall
}

func NewIndexWorker(config IndexWorkerConfig) (*IndexWorker, error) {
	if config.Catalog == nil {
		return nil, errors.New("vector index catalog is required")
	}
	if config.Owner == "" {
		return nil, errors.New("vector index worker owner is required")
	}
	if config.BuildLease <= 0 || config.ReaderLease <= 0 || config.IdleDelay <= 0 {
		return nil, errors.New("vector index worker durations must be positive")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &IndexWorker{catalog: config.Catalog, owner: config.Owner,
		buildLease: config.BuildLease, readerLease: config.ReaderLease,
		idleDelay: config.IdleDelay, clock: config.Clock,
		inflight: make(map[string]*indexBuildCall)}, nil
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

	call.record, call.err = worker.rebuild(ctx, vectorSpaceID)
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
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if active, activeErr := worker.catalog.ActiveVectorIndexGeneration(ctx, vectorSpaceID); activeErr == nil &&
		active.SourceManifestChecksum == source.ManifestChecksum {
		if validateErr := validateStoredVectorIndex(active, source, nil); validateErr == nil {
			return active, nil
		}
	}
	claim, claimed, err := worker.catalog.ClaimVectorIndexBuild(ctx, vectorSpaceID,
		source.ManifestChecksum, worker.owner, worker.clock().UTC(), worker.buildLease)
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if !claimed {
		return store.VectorIndexGenerationRecord{}, store.ErrVectorIndexBuildInProgress
	}

	sets, firstQuery, coverage, err := worker.loadVectorSets(ctx, source)
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if len(coverage.Missing) != 0 {
		return store.VectorIndexGenerationRecord{}, &unavailableVectorSourceError{coverage: coverage}
	}
	setIDs := make([]string, len(sets))
	for index, set := range sets {
		_, setIDs[index], err = document.EncodeVectorSetV1(set)
		if err != nil {
			return store.VectorIndexGenerationRecord{}, err
		}
	}
	manifest, err := vectorindex.NewManifest(setIDs)
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	built, err := vectorindex.BuildGeneration(manifest, sets, vectorindex.Options{})
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
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
	_, _ = worker.catalog.ReclaimVectorIndexGenerations(ctx, worker.clock().UTC())
	return stored, nil
}

func (worker *IndexWorker) loadVectorSets(ctx context.Context, source store.VectorIndexSource) (
	[]document.VectorSetV1, []float32, UnavailableVectorCoverage, error,
) {
	coverage := UnavailableVectorCoverage{VectorSpaceID: source.VectorSpaceID,
		SourceManifestChecksum: source.ManifestChecksum, ExternalReembeddingRequired: true}
	bySetID := make(map[string]store.VectorIndexMember, len(source.Members))
	for _, member := range source.Members {
		if _, exists := bySetID[member.VectorSetID]; !exists {
			bySetID[member.VectorSetID] = member
		}
	}
	setIDs := make([]string, 0, len(bySetID))
	for setID := range bySetID {
		setIDs = append(setIDs, setID)
	}
	slices.Sort(setIDs)
	sets := make([]document.VectorSetV1, 0, len(setIDs))
	var firstQuery []float32
	for _, setID := range setIDs {
		member := bySetID[setID]
		payload, err := worker.catalog.ReadVectorIndexVectorSet(ctx, member)
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
			return nil, nil, coverage, err
		}
		set, err := document.DecodeVectorSetV1(payload, document.VectorBounds{
			MaxRows: 100_000, MaxDimension: 16_384, MaxBytes: len(payload)})
		if err != nil {
			return nil, nil, coverage, fmt.Errorf("decoding canonical vector set %s: %w", setID, err)
		}
		canonical, checksum, err := document.EncodeVectorSetV1(set)
		if err != nil || checksum != setID || !bytes.Equal(canonical, payload) ||
			set.VectorSpaceFingerprint != source.VectorSpaceID {
			return nil, nil, coverage, errors.New("vector index source payload is not exact canonical authority")
		}
		if firstQuery == nil {
			firstQuery = append([]float32(nil), set.Vectors[0]...)
		}
		sets = append(sets, set)
	}
	return sets, firstQuery, coverage, nil
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
		if errors.Is(err, store.ErrNotFound) {
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
	for {
		spaces, err := worker.catalog.ListVectorIndexSpaces(ctx)
		if err == nil {
			for _, space := range spaces {
				_, _ = worker.Rebuild(ctx, space)
			}
		}
		timer := time.NewTimer(worker.idleDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func indexGenerationID(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func indexWorkerTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
