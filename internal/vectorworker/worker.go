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
	RetireEmptyVectorIndexHead(ctx context.Context, vectorSpaceID string, at time.Time) error
	AbandonVectorIndexBuild(ctx context.Context, claim store.VectorIndexBuildClaim, at time.Time) error
	CaptureVectorIndexSource(ctx context.Context, vectorSpaceID string) (store.VectorIndexSource, error)
	ListVectorIndexSpaces(ctx context.Context) ([]string, error)
	ClaimVectorIndexBuild(ctx context.Context, vectorSpaceID, sourceChecksum, owner string, at time.Time, lease time.Duration) (store.VectorIndexBuildClaim, bool, error)
	StageVectorIndexGeneration(ctx context.Context, claim store.VectorIndexBuildClaim, record store.VectorIndexGenerationRecord, at time.Time) error
	LoadVectorIndexGeneration(ctx context.Context, generationID string) (store.VectorIndexGenerationRecord, error)
	PublishVectorIndexGeneration(ctx context.Context, claim store.VectorIndexBuildClaim, generationID string, at time.Time) error
	ActiveVectorIndexHead(ctx context.Context, vectorSpaceID string) (store.VectorIndexHead, error)
	ActiveVectorIndexGeneration(ctx context.Context, vectorSpaceID string) (store.VectorIndexGenerationRecord, error)
	AcquireVectorIndexGeneration(ctx context.Context, vectorSpaceID, owner string, at time.Time, lease time.Duration) (store.VectorIndexReaderLease, error)
	ReleaseVectorIndexGeneration(ctx context.Context, leaseID string, fencingToken int64, at time.Time) error
	ReclaimVectorIndexGenerations(ctx context.Context, at time.Time) (int, error)
	ReplaceVectorIndexUnavailableCoverage(ctx context.Context, coverage []store.VectorIndexUnavailableCoverage) error
}

type IndexWorkerConfig struct {
	// Mutate admits short-lived work through the owner's maintenance gate.
	// An exclusively owned restore target may execute the callback directly.
	Mutate func(context.Context, func() error) error
	// Retryable classifies transient errors for Run; nil makes errors terminal.
	Retryable     func(error) bool
	ReadVectorSet func(context.Context, store.VectorIndexMember) ([]byte, error)
	Catalog       Catalog
	Owner         string
	BuildLease    time.Duration
	ReaderLease   time.Duration
	IdleDelay     time.Duration
	Clock         func() time.Time
}

type indexBuildCall struct {
	done   chan struct{}
	record store.VectorIndexGenerationRecord
	err    error
}

type IndexWorker struct {
	mutate        func(context.Context, func() error) error
	retryable     func(error) bool
	readVectorSet func(context.Context, store.VectorIndexMember) ([]byte, error)
	catalog       Catalog
	owner         string
	buildLease    time.Duration
	readerLease   time.Duration
	idleDelay     time.Duration
	clock         func() time.Time

	mu       sync.Mutex
	inflight map[string]*indexBuildCall
}

func NewIndexWorker(config IndexWorkerConfig) (*IndexWorker, error) {
	if config.Mutate == nil {
		return nil, errors.New("vector index mutation admission is required")
	}
	if config.Catalog == nil {
		return nil, errors.New("vector index catalog is required")
	}
	if config.ReadVectorSet == nil {
		return nil, errors.New("vector index payload reader is required")
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
	return &IndexWorker{mutate: config.Mutate, retryable: config.Retryable, readVectorSet: config.ReadVectorSet, catalog: config.Catalog, owner: config.Owner,
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

func (worker *IndexWorker) rebuild(ctx context.Context, vectorSpaceID string) (_ store.VectorIndexGenerationRecord, retErr error) {
	source, err := worker.catalog.CaptureVectorIndexSource(ctx, vectorSpaceID)
	if errors.Is(err, store.ErrNotFound) {
		if err := worker.mutate(ctx, func() error {
			if err := worker.catalog.RetireEmptyVectorIndexHead(ctx, vectorSpaceID, worker.clock().UTC()); err != nil {
				return err
			}
			_, err := worker.catalog.ReclaimVectorIndexGenerations(ctx, worker.clock().UTC())
			return err
		}); err != nil {
			return store.VectorIndexGenerationRecord{}, err
		}
		return store.VectorIndexGenerationRecord{}, store.ErrNotFound
	}
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if active, activeErr := worker.catalog.ActiveVectorIndexGeneration(ctx, vectorSpaceID); activeErr == nil &&
		active.SourceManifestChecksum == source.ManifestChecksum {
		if validateErr := validateStoredVectorIndex(active, source, nil); validateErr == nil {
			return active, nil
		}
	}
	var claim store.VectorIndexBuildClaim
	var claimed bool
	err = worker.mutate(ctx, func() error {
		var err error
		claim, claimed, err = worker.catalog.ClaimVectorIndexBuild(ctx, vectorSpaceID,
			source.ManifestChecksum, worker.owner, worker.clock().UTC(), worker.buildLease)
		return err
	})
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if !claimed {
		return store.VectorIndexGenerationRecord{}, store.ErrVectorIndexBuildInProgress
	}
	defer func() {
		if retErr != nil {
			// Live work waits for maintenance with its own context. Start the
			// cleanup timeout only after admission; a long repack is not a
			// failed catalog write. Cancelled work gets bounded best effort.
			admissionCtx := ctx
			if ctx.Err() != nil {
				var cancel context.CancelFunc
				admissionCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
			}
			if cleanupErr := worker.mutate(admissionCtx, func() error {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				return worker.catalog.AbandonVectorIndexBuild(cleanupCtx, claim, worker.clock().UTC())
			}); cleanupErr != nil {
				// Keep cleanup failure authoritative; lifecycle errors must not hide it.
				retErr = fmt.Errorf("abandoning failed vector index build (%v): %w", retErr, cleanupErr) //nolint:errorlint // Only cleanup remains classifiable.
			}
		}
	}()

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
	record := store.VectorIndexGenerationRecord{ID: indexGenerationID(source.ManifestChecksum, encoded),
		VectorSpaceID: vectorSpaceID, SourceManifestChecksum: source.ManifestChecksum,
		IndexManifestChecksum: metadata.Manifest.Checksum, Bytes: encoded,
		RowCount: metadata.RowCount, BuiltAt: indexWorkerTimestamp(worker.clock().UTC())}
	if err := worker.mutate(ctx, func() error {
		return worker.catalog.StageVectorIndexGeneration(ctx, claim, record, worker.clock().UTC())
	}); err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	stored, err := worker.catalog.LoadVectorIndexGeneration(ctx, record.ID)
	if err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
	if err := validateStoredVectorIndex(stored, source, firstQuery); err != nil {
		return store.VectorIndexGenerationRecord{}, errors.Join(ErrVectorIndexCandidateInvalid, err)
	}
	if err := worker.mutate(ctx, func() error {
		if err := worker.catalog.PublishVectorIndexGeneration(ctx, claim, record.ID, worker.clock().UTC()); err != nil {
			return err
		}
		if _, err := worker.catalog.ReclaimVectorIndexGenerations(ctx, worker.clock().UTC()); err != nil {
			return fmt.Errorf("reclaiming vector index generations: %w", err)
		}
		return nil
	}); err != nil {
		return store.VectorIndexGenerationRecord{}, err
	}
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
			return nil, nil, coverage, err
		}
		set, _, err := document.DecodeVectorSetV1(payload, document.VectorBounds{
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
	if indexGenerationID(source.ManifestChecksum, record.Bytes) != record.ID || record.VectorSpaceID != source.VectorSpaceID ||
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
		neighbors, err := generation.Search(smokeQuery, 1)
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
	if err := worker.mutate(ctx, func() error {
		return worker.catalog.ReplaceVectorIndexUnavailableCoverage(ctx, report.Unavailable)
	}); err != nil {
		return report, err
	}
	return report, nil
}

type IndexLease struct {
	mutate     func(context.Context, func() error) error
	catalog    Catalog
	lease      store.VectorIndexReaderLease
	opened     *vectorindex.Generation
	clock      func() time.Time
	once       sync.Once
	releaseErr error
}

func (worker *IndexWorker) Acquire(ctx context.Context, vectorSpaceID, owner string) (*IndexLease, error) {
	var lease store.VectorIndexReaderLease
	err := worker.mutate(ctx, func() error {
		var err error
		lease, err = worker.catalog.AcquireVectorIndexGeneration(ctx, vectorSpaceID, owner, worker.clock().UTC(), worker.readerLease)
		return err
	})
	if err != nil {
		return nil, err
	}
	opened, err := vectorindex.OpenGeneration(bytes.NewReader(lease.Generation.Bytes), int64(len(lease.Generation.Bytes)))
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cleanupErr := worker.mutate(cleanupCtx, func() error {
			return worker.catalog.ReleaseVectorIndexGeneration(cleanupCtx, lease.ID, lease.FencingToken, worker.clock().UTC())
		})
		return nil, errors.Join(ErrVectorIndexCandidateInvalid, err, cleanupErr)
	}
	return &IndexLease{mutate: worker.mutate, catalog: worker.catalog, lease: lease, opened: opened, clock: worker.clock}, nil
}

func (lease *IndexLease) Search(query []float32, k int) ([]vectorindex.Neighbor, error) {
	if lease == nil || lease.opened == nil {
		return nil, errors.New("vector index reader lease is not open")
	}
	return lease.opened.Search(query, k)
}

func (lease *IndexLease) Release(ctx context.Context) error {
	if lease == nil {
		return nil
	}
	lease.once.Do(func() {
		lease.releaseErr = lease.mutate(ctx, func() error {
			if err := lease.catalog.ReleaseVectorIndexGeneration(ctx, lease.lease.ID, lease.lease.FencingToken, lease.clock().UTC()); err != nil {
				return err
			}
			_, err := lease.catalog.ReclaimVectorIndexGenerations(ctx, lease.clock().UTC())
			return err
		})
	})
	return lease.releaseErr
}

func (worker *IndexWorker) Run(ctx context.Context) error {
	failures := 0
	for {
		err := worker.scan(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := worker.idleDelay
		if err != nil {
			failures++
			if worker.retryable == nil || !worker.retryable(err) || failures >= 3 {
				return err
			}
			delay = min(worker.idleDelay*time.Duration(1<<(failures-1)), 30*time.Second)
		} else {
			failures = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (worker *IndexWorker) scan(ctx context.Context) error {
	spaces, err := worker.catalog.ListVectorIndexSpaces(ctx)
	if err != nil {
		return fmt.Errorf("listing vector index spaces: %w", err)
	}
	var unavailable []UnavailableVectorCoverage
	for _, space := range spaces {
		err := worker.refresh(ctx, space)
		if missing, ok := errors.AsType[*unavailableVectorSourceError](err); ok {
			unavailable = append(unavailable, missing.coverage)
			continue
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrVectorIndexBuildInProgress) &&
			!errors.Is(err, store.ErrVectorIndexBuildFenced) && !errors.Is(err, store.ErrVectorIndexSourceStale) {
			return fmt.Errorf("rebuilding vector index %s: %w", space, err)
		}
	}
	err = worker.mutate(ctx, func() error {
		if err := worker.catalog.ReplaceVectorIndexUnavailableCoverage(ctx, unavailable); err != nil {
			return err
		}
		_, err := worker.catalog.ReclaimVectorIndexGenerations(ctx, worker.clock().UTC())
		return err
	})
	if err != nil {
		return fmt.Errorf("reclaiming vector index generations: %w", err)
	}
	return nil
}

// refresh checks immutable publication metadata before loading any generation
// bytes. Full validation remains at candidate publication and reader acquisition.
func (worker *IndexWorker) refresh(ctx context.Context, space string) error {
	source, err := worker.catalog.CaptureVectorIndexSource(ctx, space)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil {
		head, err := worker.catalog.ActiveVectorIndexHead(ctx, space)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil && head.SourceManifestChecksum == source.ManifestChecksum {
			return nil
		}
	}
	_, err = worker.Rebuild(ctx, space)
	return err
}

func indexGenerationID(sourceChecksum string, data []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("docbank-vector-index-generation/v1\x00" + sourceChecksum))
	_, _ = digest.Write(data)
	return hex.EncodeToString(digest.Sum(nil))
}

func indexWorkerTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
