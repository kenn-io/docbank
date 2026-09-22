package processing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

// IndexMutation admits the short catalog transactions used to observe and
// publish projection state through the daemon's maintenance gate.
type IndexMutation func(context.Context, func() error) error

type LexicalIndexBackendConfig struct {
	Store             *store.Store
	Mutate            IndexMutation
	RollbackRetention time.Duration
}

type lexicalIndexBackend struct {
	store             *store.Store
	mutate            IndexMutation
	rollbackRetention time.Duration
}

// NewLexicalIndexBackend adapts the existing immutable FTS generation store
// to the common freshness and targeted-repair coordinator.
func NewLexicalIndexBackend(config LexicalIndexBackendConfig) (IndexProjectionBackend, error) {
	if config.Store == nil || config.Mutate == nil {
		return nil, errors.New("lexical index store and mutation admission are required")
	}
	if config.RollbackRetention <= 0 {
		config.RollbackRetention = 24 * time.Hour
	}
	return &lexicalIndexBackend{
		store: config.Store, mutate: config.Mutate, rollbackRetention: config.RollbackRetention,
	}, nil
}

func (backend *lexicalIndexBackend) Target() IndexTarget {
	return IndexTarget{Kind: IndexLexical}
}

func (backend *lexicalIndexBackend) Inspect(ctx context.Context) (IndexProjectionSnapshot, error) {
	var projection store.LexicalIndexProjection
	err := backend.mutate(ctx, func() error {
		var err error
		projection, err = backend.store.InspectLexicalIndexProjection(ctx)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return IndexProjectionSnapshot{}, ctx.Err()
		}
		var source store.LexicalIndexSource
		var state store.IndexProjectionStateRecord
		fallbackErr := backend.mutate(ctx, func() error {
			var captureErr error
			source, captureErr = backend.store.CaptureLexicalIndexSource(ctx)
			if captureErr != nil {
				return captureErr
			}
			state, captureErr = backend.store.IndexProjectionState(
				ctx, store.IndexProjectionLexical, "",
			)
			return captureErr
		})
		if fallbackErr != nil {
			return IndexProjectionSnapshot{}, mapIndexBackendError(errors.Join(err, fallbackErr))
		}
		return IndexProjectionSnapshot{
			Target: backend.Target(), Enabled: true, Authorized: true,
			Generation: state.GenerationID, SourceWatermark: source.SourceWatermark,
			IndexWatermark:         state.IndexWatermark,
			AuthorizationWatermark: source.AuthorizationWatermark,
			Coverage:               IndexCoverage{Expected: source.Documents},
			FailureReason:          IndexFailureCorruptManifest,
		}, nil
	}
	return lexicalProjectionSnapshot(projection), nil
}

func lexicalProjectionSnapshot(projection store.LexicalIndexProjection) IndexProjectionSnapshot {
	generation := projection.State.GenerationID
	if projection.Serving {
		generation = projection.Generation.ID
	}
	return IndexProjectionSnapshot{
		Target: IndexTarget{Kind: IndexLexical}, Enabled: true, Authorized: true,
		Generation: generation, SourceWatermark: projection.Source.SourceWatermark,
		IndexWatermark:         projection.State.IndexWatermark,
		AuthorizationWatermark: projection.Source.AuthorizationWatermark,
		Coverage: IndexCoverage{
			Expected: projection.Source.Documents, Indexed: projection.State.IndexedCount,
			Unavailable: projection.State.UnavailableCount,
		},
	}
}

func (backend *lexicalIndexBackend) Preview(
	ctx context.Context, _ IndexProjectionSnapshot,
) (IndexRepairWork, error) {
	var source store.LexicalIndexSource
	err := backend.mutate(ctx, func() error {
		var err error
		source, err = backend.store.CaptureLexicalIndexSource(ctx)
		return err
	})
	if err != nil {
		return IndexRepairWork{}, mapIndexBackendError(err)
	}
	return IndexRepairWork{Documents: source.Documents, Bytes: source.Bytes}, nil
}

func (backend *lexicalIndexBackend) Build(
	ctx context.Context, request IndexBuildRequest,
) (IndexCandidate, error) {
	var candidate store.LexicalIndexCandidate
	err := backend.mutate(ctx, func() error {
		source, err := backend.store.CaptureLexicalIndexSource(ctx)
		if err != nil {
			return err
		}
		if source.SourceWatermark != request.SourceWatermark {
			return ErrIndexSourceChanged
		}
		if source.AuthorizationWatermark != request.AuthorizationWatermark {
			return ErrIndexPermissionChanged
		}
		candidate, err = backend.store.StageLexicalIndexGeneration(ctx, source)
		return err
	})
	if err != nil {
		return IndexCandidate{}, mapIndexBackendError(err)
	}
	return IndexCandidate{
		Target: backend.Target(), Generation: candidate.Generation.ID, Handle: candidate,
	}, nil
}

func (backend *lexicalIndexBackend) Validate(ctx context.Context, candidate IndexCandidate) error {
	stored, ok := candidate.Handle.(store.LexicalIndexCandidate)
	if !ok || candidate.Target != backend.Target() || candidate.Generation != stored.Generation.ID {
		return ErrIndexManifestCorrupt
	}
	return mapIndexBackendError(backend.store.ValidateLexicalIndexGeneration(ctx, stored))
}

func (backend *lexicalIndexBackend) Commit(
	ctx context.Context, candidate IndexCandidate, request IndexBuildRequest,
) (IndexProjectionSnapshot, error) {
	stored, ok := candidate.Handle.(store.LexicalIndexCandidate)
	if !ok || candidate.Target != backend.Target() || candidate.Generation != stored.Generation.ID {
		return IndexProjectionSnapshot{}, ErrIndexManifestCorrupt
	}
	if stored.Source.SourceWatermark != request.SourceWatermark ||
		stored.Source.AuthorizationWatermark != request.AuthorizationWatermark {
		return IndexProjectionSnapshot{}, ErrIndexPlanChanged
	}
	var projection store.LexicalIndexProjection
	err := backend.mutate(ctx, func() error {
		if err := backend.store.PublishLexicalIndexGeneration(
			ctx, stored, time.Now().UTC(), backend.rollbackRetention,
		); err != nil {
			return err
		}
		var err error
		projection, err = backend.store.InspectLexicalIndexProjection(ctx)
		return err
	})
	if err != nil {
		return IndexProjectionSnapshot{}, mapIndexBackendError(err)
	}
	return lexicalProjectionSnapshot(projection), nil
}

func (backend *lexicalIndexBackend) Discard(ctx context.Context, candidate IndexCandidate) error {
	stored, ok := candidate.Handle.(store.LexicalIndexCandidate)
	if !ok {
		return ErrIndexManifestCorrupt
	}
	err := backend.mutate(ctx, func() error {
		return backend.store.DiscardLexicalIndexGeneration(ctx, stored.Generation.ID)
	})
	return mapIndexBackendError(err)
}

type VectorIndexBackendConfig struct {
	Store             *store.Store
	Worker            *vectorworker.IndexWorker
	Mutate            IndexMutation
	VectorSpaceID     string
	RollbackRetention time.Duration
}

type vectorIndexBackend struct {
	store             *store.Store
	worker            *vectorworker.IndexWorker
	mutate            IndexMutation
	vectorSpaceID     string
	rollbackRetention time.Duration
}

// NewVectorIndexBackend adapts one vector space. Rebuilds consume only
// retained canonical vector sets and therefore disclose no provider work.
func NewVectorIndexBackend(config VectorIndexBackendConfig) (IndexProjectionBackend, error) {
	if config.Store == nil || config.Worker == nil || config.Mutate == nil || config.VectorSpaceID == "" {
		return nil, errors.New("vector index store, worker, mutation admission, and space are required")
	}
	if config.RollbackRetention <= 0 {
		config.RollbackRetention = 24 * time.Hour
	}
	return &vectorIndexBackend{
		store: config.Store, worker: config.Worker, mutate: config.Mutate,
		vectorSpaceID: config.VectorSpaceID, rollbackRetention: config.RollbackRetention,
	}, nil
}

func (backend *vectorIndexBackend) Target() IndexTarget {
	return IndexTarget{Kind: IndexVector, Key: backend.vectorSpaceID}
}

func (backend *vectorIndexBackend) inspectProjection(
	ctx context.Context,
) (store.VectorIndexProjection, error) {
	var projection store.VectorIndexProjection
	err := backend.mutate(ctx, func() error {
		var err error
		projection, err = backend.store.InspectVectorIndexProjection(ctx, backend.vectorSpaceID)
		return err
	})
	if err != nil {
		return store.VectorIndexProjection{}, mapIndexBackendError(err)
	}
	return projection, nil
}

func (backend *vectorIndexBackend) Inspect(ctx context.Context) (IndexProjectionSnapshot, error) {
	projection, err := backend.inspectProjection(ctx)
	if err != nil {
		return IndexProjectionSnapshot{}, err
	}
	snapshot := vectorProjectionSnapshot(backend.Target(), projection)
	if projection.Serving &&
		projection.Generation.SourceManifestChecksum == projection.Source.ManifestChecksum {
		if err := backend.worker.ValidateStoredGeneration(projection.Generation, projection.Source); err != nil {
			snapshot.FailureReason = IndexFailureCorruptManifest
		}
	}
	return snapshot, nil
}

func vectorProjectionSnapshot(
	target IndexTarget, projection store.VectorIndexProjection,
) IndexProjectionSnapshot {
	indexed := min(projection.State.IndexedCount, projection.Documents)
	unavailable := min(projection.State.UnavailableCount, projection.Documents-indexed)
	generation := projection.State.GenerationID
	if projection.Serving {
		generation = projection.Generation.ID
	}
	return IndexProjectionSnapshot{
		Target: target, Enabled: true, Authorized: true, Generation: generation,
		SourceWatermark:        projection.State.SourceWatermark,
		IndexWatermark:         projection.State.IndexWatermark,
		AuthorizationWatermark: projection.State.AuthorizationWatermark,
		Coverage: IndexCoverage{
			Expected: projection.Documents, Indexed: indexed, Unavailable: unavailable,
		},
	}
}

func (backend *vectorIndexBackend) Preview(
	ctx context.Context, _ IndexProjectionSnapshot,
) (IndexRepairWork, error) {
	projection, err := backend.inspectProjection(ctx)
	if err != nil {
		return IndexRepairWork{}, err
	}
	return IndexRepairWork{Documents: projection.Documents, Bytes: projection.Bytes}, nil
}

func (backend *vectorIndexBackend) Build(
	ctx context.Context, request IndexBuildRequest,
) (IndexCandidate, error) {
	projection, err := backend.inspectProjection(ctx)
	if err != nil {
		return IndexCandidate{}, err
	}
	if projection.State.SourceWatermark != request.SourceWatermark {
		return IndexCandidate{}, ErrIndexSourceChanged
	}
	if projection.State.AuthorizationWatermark != request.AuthorizationWatermark {
		return IndexCandidate{}, ErrIndexPermissionChanged
	}
	candidate, err := backend.worker.PrepareRepair(ctx, backend.vectorSpaceID)
	if err != nil {
		return IndexCandidate{}, mapIndexBackendError(err)
	}
	if candidate.Source.ManifestChecksum != projection.Source.ManifestChecksum {
		_ = backend.worker.DiscardRepairCandidate(context.WithoutCancel(ctx), candidate)
		return IndexCandidate{}, ErrIndexSourceChanged
	}
	return IndexCandidate{
		Target: backend.Target(), Generation: candidate.Record.ID, Handle: candidate,
	}, nil
}

func (backend *vectorIndexBackend) Validate(ctx context.Context, candidate IndexCandidate) error {
	staged, ok := candidate.Handle.(vectorworker.RepairCandidate)
	if !ok || candidate.Target != backend.Target() || candidate.Generation != staged.Record.ID {
		return ErrIndexManifestCorrupt
	}
	if err := backend.worker.ValidateRepairCandidate(ctx, staged); err != nil {
		return errors.Join(ErrIndexManifestCorrupt, err)
	}
	return nil
}

func (backend *vectorIndexBackend) Commit(
	ctx context.Context, candidate IndexCandidate, request IndexBuildRequest,
) (IndexProjectionSnapshot, error) {
	staged, ok := candidate.Handle.(vectorworker.RepairCandidate)
	if !ok || candidate.Target != backend.Target() || candidate.Generation != staged.Record.ID {
		return IndexProjectionSnapshot{}, ErrIndexManifestCorrupt
	}
	projection, err := backend.inspectProjection(ctx)
	if err != nil {
		return IndexProjectionSnapshot{}, err
	}
	if projection.State.SourceWatermark != request.SourceWatermark ||
		projection.Source.ManifestChecksum != staged.Source.ManifestChecksum {
		return IndexProjectionSnapshot{}, ErrIndexSourceChanged
	}
	if projection.State.AuthorizationWatermark != request.AuthorizationWatermark {
		return IndexProjectionSnapshot{}, ErrIndexPermissionChanged
	}
	if err := backend.worker.PublishRepairCandidate(ctx, staged, backend.rollbackRetention); err != nil {
		return IndexProjectionSnapshot{}, mapIndexBackendError(err)
	}
	projection, err = backend.inspectProjection(ctx)
	if err != nil {
		return IndexProjectionSnapshot{}, err
	}
	return vectorProjectionSnapshot(backend.Target(), projection), nil
}

func (backend *vectorIndexBackend) Discard(ctx context.Context, candidate IndexCandidate) error {
	staged, ok := candidate.Handle.(vectorworker.RepairCandidate)
	if !ok {
		return ErrIndexManifestCorrupt
	}
	return mapIndexBackendError(backend.worker.DiscardRepairCandidate(ctx, staged))
}

type disabledIndexBackend struct{ target IndexTarget }

// NewDisabledIndexBackend makes an unavailable projection explicit in status
// rather than pretending an unmaterialized query path can be repaired.
func NewDisabledIndexBackend(target IndexTarget) (IndexProjectionBackend, error) {
	if err := validateIndexTarget(target); err != nil {
		return nil, err
	}
	return disabledIndexBackend{target: target}, nil
}

func (backend disabledIndexBackend) Target() IndexTarget { return backend.target }
func (backend disabledIndexBackend) Inspect(context.Context) (IndexProjectionSnapshot, error) {
	return IndexProjectionSnapshot{Target: backend.target}, nil
}
func (backend disabledIndexBackend) Preview(context.Context, IndexProjectionSnapshot) (IndexRepairWork, error) {
	return IndexRepairWork{}, ErrIndexDisabled
}
func (backend disabledIndexBackend) Build(context.Context, IndexBuildRequest) (IndexCandidate, error) {
	return IndexCandidate{}, ErrIndexDisabled
}
func (backend disabledIndexBackend) Validate(context.Context, IndexCandidate) error {
	return ErrIndexDisabled
}
func (backend disabledIndexBackend) Commit(context.Context, IndexCandidate, IndexBuildRequest) (IndexProjectionSnapshot, error) {
	return IndexProjectionSnapshot{}, ErrIndexDisabled
}
func (backend disabledIndexBackend) Discard(context.Context, IndexCandidate) error { return nil }

func mapIndexBackendError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, ErrIndexSourceChanged), errors.Is(err, ErrIndexPermissionChanged),
		errors.Is(err, ErrIndexPlanChanged), errors.Is(err, ErrIndexManifestCorrupt):
		return err
	case errors.Is(err, store.ErrLexicalGenerationStale), errors.Is(err, store.ErrVectorIndexSourceStale):
		return errors.Join(ErrIndexSourceChanged, err)
	case errors.Is(err, store.ErrVectorIndexBuildInProgress):
		return errors.Join(ErrIndexRepairInProgress, err)
	case errors.Is(err, vectorworker.ErrVectorIndexCandidateInvalid):
		return errors.Join(ErrIndexManifestCorrupt, err)
	case errors.Is(err, store.ErrNotFound):
		return errors.Join(ErrIndexManifestCorrupt, err)
	case strings.Contains(strings.ToLower(err.Error()), "disk is full"):
		return errors.Join(ErrIndexStorageFull, err)
	default:
		return fmt.Errorf("%w: %w", ErrIndexBuildFailed, err)
	}
}
