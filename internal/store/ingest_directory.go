package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// IngestDirectoryPlan is an authority-free directory coordinate anchored at
// the deepest live directory identity observed during preparation.
type IngestDirectoryPlan struct {
	anchorID int64
	segments []string
}

// IngestDirectoryResolution identifies the directory prefixes resolved by one
// committed ingest mutation.
type IngestDirectoryResolution struct {
	anchorID int64
	segments []string
	nodeIDs  []int64
}

// ResolvedID returns the stable directory identity when the whole plan was
// already resolved.
func (p IngestDirectoryPlan) ResolvedID() (int64, bool) {
	return p.anchorID, len(p.segments) == 0
}

// Rebase anchors plan at the deepest directory identity shared with this
// committed resolution and retains any still-unresolved suffix.
func (r IngestDirectoryResolution) Rebase(plan IngestDirectoryPlan) IngestDirectoryPlan {
	if len(plan.segments) == 0 || plan.anchorID != r.anchorID {
		return plan
	}
	common := 0
	for common < len(plan.segments) && common < len(r.segments) &&
		plan.segments[common] == r.segments[common] {
		common++
	}
	if common == 0 {
		return plan
	}
	return IngestDirectoryPlan{
		anchorID: r.nodeIDs[common-1],
		segments: append([]string(nil), plan.segments[common:]...),
	}
}

// PrepareIngestDirectory resolves as much of path as currently exists without
// creating metadata authority.
func (s *Store) PrepareIngestDirectory(
	ctx context.Context, path string,
) (IngestDirectoryPlan, error) {
	segments, err := normalizeIngestDirectorySegments(path, splitPath(path))
	if err != nil {
		return IngestDirectoryPlan{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return IngestDirectoryPlan{}, fmt.Errorf("beginning ingest directory snapshot: %w", err)
	}
	plan, err := prepareIngestDirectoryTx(ctx, tx, s.rootID, segments)
	if err != nil {
		_ = tx.Rollback()
		return IngestDirectoryPlan{}, err
	}
	if err := tx.Commit(); err != nil {
		return IngestDirectoryPlan{}, fmt.Errorf("closing ingest directory snapshot: %w", err)
	}
	return plan, nil
}

// ExtendIngestDirectory resolves existing child names beneath a stable plan
// without creating metadata authority.
func (s *Store) ExtendIngestDirectory(
	ctx context.Context, plan IngestDirectoryPlan, names ...string,
) (IngestDirectoryPlan, error) {
	normalized, err := normalizeIngestDirectorySegments("ingest child", names)
	if err != nil {
		return IngestDirectoryPlan{}, err
	}
	if err := validateIngestDirectoryPlan(plan); err != nil {
		return IngestDirectoryPlan{}, err
	}
	if len(plan.segments) != 0 {
		plan.segments = append(append([]string(nil), plan.segments...), normalized...)
		return plan, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return IngestDirectoryPlan{}, fmt.Errorf("beginning ingest directory snapshot: %w", err)
	}
	extended, err := prepareIngestDirectoryTx(ctx, tx, plan.anchorID, normalized)
	if err != nil {
		_ = tx.Rollback()
		return IngestDirectoryPlan{}, err
	}
	if err := tx.Commit(); err != nil {
		return IngestDirectoryPlan{}, fmt.Errorf("closing ingest directory snapshot: %w", err)
	}
	return extended, nil
}

func normalizeIngestDirectorySegments(path string, segments []string) ([]string, error) {
	normalized := make([]string, len(segments))
	for i, segment := range segments {
		name, err := NormalizeName(segment)
		if err != nil {
			return nil, fmt.Errorf("path %q: %w", path, err)
		}
		normalized[i] = name
	}
	return normalized, nil
}

func validateIngestDirectoryPlan(plan IngestDirectoryPlan) error {
	if plan.anchorID <= 0 {
		return errors.New("ingest directory plan has no anchor")
	}
	for _, segment := range plan.segments {
		if _, err := NormalizeName(segment); err != nil {
			return fmt.Errorf("invalid ingest directory plan: %w", err)
		}
	}
	return nil
}

func prepareIngestDirectoryTx(
	ctx context.Context, tx *sql.Tx, anchorID int64, segments []string,
) (IngestDirectoryPlan, error) {
	anchor, err := liveDirTx(tx, anchorID)
	if err != nil {
		return IngestDirectoryPlan{}, err
	}
	for i, segment := range segments {
		next, err := childByName(ctx, tx, anchor.ID, segment)
		switch {
		case err == nil:
			if !next.IsDir() {
				return IngestDirectoryPlan{}, fmt.Errorf("path segment %q: %w", segment, ErrNotDir)
			}
			anchor = next
		case errors.Is(err, ErrNotFound):
			return IngestDirectoryPlan{
				anchorID: anchor.ID,
				segments: append([]string(nil), segments[i:]...),
			}, nil
		default:
			return IngestDirectoryPlan{}, err
		}
	}
	return IngestDirectoryPlan{anchorID: anchor.ID}, nil
}

func (s *Store) ensureIngestDirectoryTx(
	ctx context.Context, tx *sql.Tx, plan IngestDirectoryPlan,
) (Node, IngestDirectoryResolution, error) {
	if err := validateIngestDirectoryPlan(plan); err != nil {
		return Node{}, IngestDirectoryResolution{}, err
	}
	leaf, err := liveDirTx(tx, plan.anchorID)
	if err != nil {
		return Node{}, IngestDirectoryResolution{}, err
	}
	resolution := IngestDirectoryResolution{
		anchorID: plan.anchorID,
		segments: append([]string(nil), plan.segments...),
		nodeIDs:  make([]int64, 0, len(plan.segments)),
	}
	for _, segment := range plan.segments {
		next, childErr := childByName(ctx, tx, leaf.ID, segment)
		switch {
		case childErr == nil:
			if !next.IsDir() {
				return Node{}, IngestDirectoryResolution{}, fmt.Errorf(
					"path segment %q: %w", segment, ErrNotDir,
				)
			}
		case errors.Is(childErr, ErrNotFound):
			next, childErr = s.mkdirNodeTx(ctx, tx, leaf.ID, segment)
			if childErr != nil {
				return Node{}, IngestDirectoryResolution{}, childErr
			}
		default:
			return Node{}, IngestDirectoryResolution{}, childErr
		}
		leaf = next
		resolution.nodeIDs = append(resolution.nodeIDs, leaf.ID)
	}
	return leaf, resolution, nil
}

type initialIngestAdmissionError struct{ cause error }

func (e initialIngestAdmissionError) Error() string { return e.cause.Error() }
func (e initialIngestAdmissionError) Unwrap() error { return e.cause }

// IsInitialIngestAdmissionError reports whether err rejected the run's initial
// label before the surrounding ingest mutation could publish authority.
func IsInitialIngestAdmissionError(err error) bool {
	var admission initialIngestAdmissionError
	return errors.As(err, &admission)
}

func (s *Store) ensureIngestRunForMutationTx(
	ctx context.Context, tx *sql.Tx, run IngestRun,
) (bool, error) {
	inserted, err := ensureIngestRunTx(ctx, tx, run)
	if err == nil || run.initialLabel == nil {
		return inserted, err
	}
	if s.driver.IsUniqueViolation(err) {
		return false, initialIngestAdmissionError{cause: fmt.Errorf(
			"collection label %q: %w", *run.initialLabel, ErrExists,
		)}
	}
	if errors.Is(err, ErrAuditMutationUnsupported) {
		return false, initialIngestAdmissionError{cause: err}
	}
	return false, err
}

// IngestDirectoryError identifies the failed plan in a finalization batch.
type IngestDirectoryError struct {
	Index int
	Err   error
}

func (e *IngestDirectoryError) Error() string { return e.Err.Error() }
func (e *IngestDirectoryError) Unwrap() error { return e.Err }

// FinalizeIngestDirectories commits a labeled filesystem import's remaining
// directories. A zero-document run is admitted inside the transaction and
// removed again before commit, so no collection receipt is published.
func (s *Store) FinalizeIngestDirectories(
	ctx context.Context, run IngestRun, plans []IngestDirectoryPlan,
) ([]IngestDirectoryResolution, error) {
	if run.initialLabel == nil {
		return nil, errors.New("finalizing ingest directories requires an initial label")
	}
	for index, plan := range plans {
		if err := validateIngestDirectoryPlan(plan); err != nil {
			return nil, &IngestDirectoryError{Index: index, Err: err}
		}
	}
	resolutions := make([]IngestDirectoryResolution, 0, len(plans))
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		inserted, err := s.ensureIngestRunForMutationTx(ctx, tx, run)
		if err != nil {
			return err
		}
		for index, plan := range plans {
			_, resolution, err := s.ensureIngestDirectoryTx(ctx, tx, plan)
			if err != nil {
				return &IngestDirectoryError{Index: index, Err: err}
			}
			resolutions = append(resolutions, resolution)
		}
		if !inserted {
			return nil
		}
		labelResult, err := tx.ExecContext(ctx,
			`DELETE FROM collection_labels WHERE ingest_id=?`, run.ID(),
		)
		if err != nil {
			return fmt.Errorf("removing zero-document collection label: %w", err)
		}
		labelRows, err := labelResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking zero-document collection label removal: %w", err)
		}
		if labelRows != 1 {
			return fmt.Errorf("removing zero-document collection label changed %d rows", labelRows)
		}
		runResult, err := tx.ExecContext(ctx, `DELETE FROM ingests WHERE id=?`, run.ID())
		if err != nil {
			return fmt.Errorf("removing zero-document ingest: %w", err)
		}
		runRows, err := runResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking zero-document ingest removal: %w", err)
		}
		if runRows != 1 {
			return fmt.Errorf("removing zero-document ingest changed %d rows", runRows)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resolutions, nil
}
