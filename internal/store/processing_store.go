package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sort"
)

var (
	ErrInvalidProcessingSourceFence       = errors.New("invalid processing source fence")
	ErrProcessingSourceFenceStaleVersion  = errors.New("content version is not current and live")
	ErrProcessingSourceFenceScopeTooLarge = errors.New("processing source fence scope is too large")
)

// ProcessingSourceFenceScopeError reports the complete observed population
// when no exact bounded fence can be returned.
type ProcessingSourceFenceScopeError struct {
	ObservedScopeCount int
}

func (e *ProcessingSourceFenceScopeError) Error() string {
	return fmt.Sprintf("%v: observed %d current live content versions; narrow the source scope",
		ErrProcessingSourceFenceScopeTooLarge, e.ObservedScopeCount)
}

func (e *ProcessingSourceFenceScopeError) Unwrap() error {
	return ErrProcessingSourceFenceScopeTooLarge
}

// ProcessingSourceFenceRequest selects exactly one explicit or metadata-filter mode.
type ProcessingSourceFenceRequest struct {
	ContentVersionIDs []string
	Filters           *SearchOptions
}

// ProcessingSourceFenceResolution is exact current/live authority from one read snapshot.
type ProcessingSourceFenceResolution struct {
	ContentVersionIDs  []string
	ObservedScopeCount int
}

// ResolveProcessingSourceFence resolves one bounded exact search population.
func (s *Store) ResolveProcessingSourceFence(
	ctx context.Context, request ProcessingSourceFenceRequest,
) (ProcessingSourceFenceResolution, error) {
	explicit := len(request.ContentVersionIDs) != 0
	if explicit == (request.Filters != nil) {
		return ProcessingSourceFenceResolution{}, fmt.Errorf(
			"%w: select exactly one request mode", ErrInvalidProcessingSourceFence)
	}
	if explicit {
		ids := slices.Clone(request.ContentVersionIDs)
		sort.Strings(ids)
		for index, id := range ids {
			if err := validateUUIDv4(id); err != nil {
				return ProcessingSourceFenceResolution{}, fmt.Errorf(
					"%w: content version ID is invalid", ErrInvalidProcessingSourceFence)
			}
			if index > 0 && ids[index-1] == id {
				return ProcessingSourceFenceResolution{}, fmt.Errorf(
					"%w: content version IDs must be unique", ErrInvalidProcessingSourceFence)
			}
		}
		if len(ids) > MaxSearchSourceFenceIDs {
			return ProcessingSourceFenceResolution{}, &ProcessingSourceFenceScopeError{
				ObservedScopeCount: len(ids),
			}
		}
		return s.resolveExplicitProcessingSourceFence(ctx, ids)
	}
	if len(request.Filters.ContentVersionIDs) != 0 {
		return ProcessingSourceFenceResolution{}, fmt.Errorf(
			"%w: metadata filters cannot contain a source fence", ErrInvalidProcessingSourceFence)
	}
	return s.resolveFilteredProcessingSourceFence(ctx, *request.Filters)
}

func (s *Store) resolveExplicitProcessingSourceFence(
	ctx context.Context, ids []string,
) (ProcessingSourceFenceResolution, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProcessingSourceFenceResolution{}, fmt.Errorf("starting source-fence snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	encoded, err := json.Marshal(ids)
	if err != nil {
		return ProcessingSourceFenceResolution{}, errors.New("encoding source-fence identities")
	}
	rows, err := tx.QueryContext(ctx, `
		WITH requested(version_id) AS (SELECT value FROM json_each(?))
		SELECT requested.version_id,
		       cv.version_id IS NOT NULL,
		       COALESCE(n.current_version_id=requested.version_id AND n.trashed_at IS NULL,0)
		FROM requested
		LEFT JOIN content_versions cv ON cv.version_id=requested.version_id
		LEFT JOIN nodes n ON n.id=cv.node_id
		ORDER BY requested.version_id`, string(encoded))
	if err != nil {
		return ProcessingSourceFenceResolution{}, errors.New("resolving source-fence identities")
	}
	defer func() { _ = rows.Close() }()
	observed := 0
	for rows.Next() {
		var id string
		var exists, currentLive bool
		if err := rows.Scan(&id, &exists, &currentLive); err != nil {
			return ProcessingSourceFenceResolution{}, errors.New("reading source-fence identities")
		}
		observed++
		if !exists {
			return ProcessingSourceFenceResolution{}, fmt.Errorf("content version: %w", ErrNotFound)
		}
		if !currentLive {
			return ProcessingSourceFenceResolution{}, ErrProcessingSourceFenceStaleVersion
		}
	}
	if err := rows.Err(); err != nil {
		return ProcessingSourceFenceResolution{}, errors.New("reading source-fence identities")
	}
	if observed != len(ids) {
		return ProcessingSourceFenceResolution{}, errors.New("source-fence identity count disagrees")
	}
	if err := tx.Commit(); err != nil {
		return ProcessingSourceFenceResolution{}, errors.New("closing source-fence snapshot")
	}
	return ProcessingSourceFenceResolution{ContentVersionIDs: ids, ObservedScopeCount: observed}, nil
}

func (s *Store) resolveFilteredProcessingSourceFence(
	ctx context.Context, filters SearchOptions,
) (ProcessingSourceFenceResolution, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProcessingSourceFenceResolution{}, fmt.Errorf("starting source-fence snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	resolved, err := s.resolveFilteredProcessingSourceFenceSnapshot(ctx, tx, filters)
	if err != nil {
		return ProcessingSourceFenceResolution{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProcessingSourceFenceResolution{}, errors.New("closing source-fence snapshot")
	}
	return resolved, nil
}

func (s *Store) resolveFilteredProcessingSourceFenceSnapshot(
	ctx context.Context, snapshot *sql.Tx, filters SearchOptions,
) (ProcessingSourceFenceResolution, error) {
	normalized, err := s.normalizeSearchOptionsWithQuerier(ctx, snapshot, filters)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotDir) {
			return ProcessingSourceFenceResolution{}, err
		}
		return ProcessingSourceFenceResolution{}, fmt.Errorf("%w: %w",
			ErrInvalidProcessingSourceFence, err)
	}
	filterSQL, args := searchFilterSQL(normalized)
	var observed int
	if err := snapshot.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+nodeFrom+`
		WHERE n.kind='file' AND n.trashed_at IS NULL `+filterSQL, args...).Scan(&observed); err != nil {
		return ProcessingSourceFenceResolution{}, errors.New("counting source-fence scope")
	}
	if observed > MaxSearchSourceFenceIDs {
		return ProcessingSourceFenceResolution{}, &ProcessingSourceFenceScopeError{
			ObservedScopeCount: observed,
		}
	}
	rows, err := snapshot.QueryContext(ctx, `SELECT cv.version_id FROM `+nodeFrom+`
		WHERE n.kind='file' AND n.trashed_at IS NULL `+filterSQL+`
		ORDER BY cv.version_id`, args...)
	if err != nil {
		return ProcessingSourceFenceResolution{}, errors.New("resolving source-fence scope")
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0, observed)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ProcessingSourceFenceResolution{}, errors.New("reading source-fence scope")
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return ProcessingSourceFenceResolution{}, errors.New("reading source-fence scope")
	}
	if len(ids) != observed {
		return ProcessingSourceFenceResolution{}, errors.New("source-fence scope count disagrees")
	}
	return ProcessingSourceFenceResolution{ContentVersionIDs: ids, ObservedScopeCount: observed}, nil
}
