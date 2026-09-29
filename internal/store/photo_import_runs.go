package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

const (
	PhotoImportStateRunning         = "running"
	PhotoImportStateCancelRequested = "cancel-requested"
	PhotoImportStateCompleted       = "completed"
	PhotoImportStateCancelled       = "cancelled"
	PhotoImportStateFailed          = "failed"
	PhotoImportStateInterrupted     = "interrupted"
	PhotoImportStateAmbiguous       = "ambiguous"
	maxPhotoImportTextBytes         = 16 << 10
)

type PhotoImportRun struct {
	ID              string                 `json:"id"`
	Revision        int64                  `json:"revision"`
	State           string                 `json:"state"`
	SourceRoot      string                 `json:"source_root"`
	Destination     string                 `json:"destination"`
	TotalGroups     int64                  `json:"total_groups"`
	CompletedGroups int64                  `json:"completed_groups"`
	AddedGroups     int64                  `json:"added_groups"`
	SkippedGroups   int64                  `json:"skipped_groups"`
	FailedGroups    int64                  `json:"failed_groups"`
	AmbiguousGroups int64                  `json:"ambiguous_groups"`
	CancelRequested bool                   `json:"cancel_requested"`
	Error           string                 `json:"error,omitzero"`
	Ambiguities     []PhotoImportAmbiguity `json:"ambiguities,omitzero"`
	StartedAt       string                 `json:"started_at"`
	UpdatedAt       string                 `json:"updated_at"`
	FinishedAt      string                 `json:"finished_at,omitzero"`
}

func photoImportStateValid(state string) bool {
	switch state {
	case PhotoImportStateRunning, PhotoImportStateCancelRequested,
		PhotoImportStateCompleted, PhotoImportStateCancelled,
		PhotoImportStateFailed, PhotoImportStateInterrupted,
		PhotoImportStateAmbiguous:
		return true
	default:
		return false
	}
}

func photoImportStateTerminal(state string) bool {
	switch state {
	case PhotoImportStateCompleted, PhotoImportStateCancelled,
		PhotoImportStateFailed, PhotoImportStateInterrupted,
		PhotoImportStateAmbiguous:
		return true
	default:
		return false
	}
}

func validatePhotoImportRun(run PhotoImportRun) error {
	if validateUUIDv4(run.ID) != nil || run.Revision < 1 || !photoImportStateValid(run.State) ||
		run.SourceRoot == "" || run.Destination == "" || run.TotalGroups < 0 ||
		run.CompletedGroups < 0 || run.AddedGroups < 0 || run.SkippedGroups < 0 ||
		run.FailedGroups < 0 || run.AmbiguousGroups < 0 || run.CompletedGroups > run.TotalGroups {
		return errors.New("invalid photo import run")
	}
	if len(run.Error) > maxPhotoImportTextBytes {
		return errors.New("photo import run error is too large")
	}
	if err := validateMetadataTime("photo import run started_at", run.StartedAt); err != nil {
		return err
	}
	if err := validateMetadataTime("photo import run updated_at", run.UpdatedAt); err != nil {
		return err
	}
	if photoImportStateTerminal(run.State) != (run.FinishedAt != "") {
		return errors.New("photo import run terminal timestamp does not match state")
	}
	if run.FinishedAt != "" {
		if err := validateMetadataTime("photo import run finished_at", run.FinishedAt); err != nil {
			return err
		}
	}
	return nil
}

func marshalPhotoImportAmbiguities(values []PhotoImportAmbiguity) (any, error) {
	if len(values) == 0 {
		return (*string)(nil), nil
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPhotoImportTextBytes {
		return nil, errors.New("photo import ambiguity summary is too large")
	}
	return string(raw), nil
}

func unmarshalPhotoImportAmbiguities(raw sql.NullString) ([]PhotoImportAmbiguity, error) {
	if !raw.Valid || raw.String == "" {
		return []PhotoImportAmbiguity(nil), nil
	}
	var values []PhotoImportAmbiguity
	if err := json.Unmarshal([]byte(raw.String), &values); err != nil {
		return nil, fmt.Errorf("decoding photo import ambiguities: %w", err)
	}
	for _, value := range values {
		if value.GroupKey == "" || len(value.Candidates) == 0 {
			return nil, errors.New("invalid photo import ambiguity")
		}
	}
	return values, nil
}

func scanPhotoImportRun(row interface {
	Scan(dest ...any) error
}) (PhotoImportRun, error) {
	var run PhotoImportRun
	var cancel int64
	var errorText, ambiguityJSON, finished sql.NullString
	if err := row.Scan(&run.ID, &run.Revision, &run.State, &run.SourceRoot, &run.Destination,
		&run.TotalGroups, &run.CompletedGroups, &run.AddedGroups, &run.SkippedGroups,
		&run.FailedGroups, &run.AmbiguousGroups, &cancel, &errorText, &ambiguityJSON,
		&run.StartedAt, &run.UpdatedAt, &finished); err != nil {
		return PhotoImportRun{}, err
	}
	run.CancelRequested = cancel != 0
	if errorText.Valid {
		run.Error = errorText.String
	}
	if finished.Valid {
		run.FinishedAt = finished.String
	}
	var err error
	run.Ambiguities, err = unmarshalPhotoImportAmbiguities(ambiguityJSON)
	if err != nil {
		return PhotoImportRun{}, err
	}
	if err := validatePhotoImportRun(run); err != nil {
		return PhotoImportRun{}, err
	}
	return run, nil
}

const photoImportRunColumns = `run_id, revision, state, source_root, destination,
 total_groups, completed_groups, added_groups, skipped_groups, failed_groups,
 ambiguous_groups, cancel_requested, error, ambiguity_json, started_at,
 updated_at, finished_at`

func (s *Store) photoImportRunTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id string) (PhotoImportRun, error) {
	if validateUUIDv4(id) != nil {
		return PhotoImportRun{}, ErrNotFound
	}
	run, err := scanPhotoImportRun(q.QueryRowContext(ctx,
		`SELECT `+photoImportRunColumns+` FROM photo_import_runs WHERE run_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoImportRun{}, ErrNotFound
	}
	if err != nil {
		return PhotoImportRun{}, fmt.Errorf("reading photo import run %s: %w", id, err)
	}
	return run, nil
}

func (s *Store) StartPhotoImportRun(ctx context.Context, sourceRoot, destination string, totalGroups int64) (PhotoImportRun, error) {
	if sourceRoot == "" || destination == "" || totalGroups < 0 {
		return PhotoImportRun{}, errors.New("photo import source root, destination, and group count are required")
	}
	id, err := newUUIDv4()
	if err != nil {
		return PhotoImportRun{}, err
	}
	now := nowRFC3339()
	run := PhotoImportRun{ID: id, Revision: 1, State: PhotoImportStateRunning,
		SourceRoot: sourceRoot, Destination: destination, TotalGroups: totalGroups,
		StartedAt: now, UpdatedAt: now}
	if err := validatePhotoImportRun(run); err != nil {
		return PhotoImportRun{}, err
	}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_import_runs(
			run_id,revision,state,source_root,destination,total_groups,started_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?)`, run.ID, run.Revision, run.State, run.SourceRoot,
			run.Destination, run.TotalGroups, run.StartedAt, run.UpdatedAt)
		return err
	})
	if err != nil {
		return PhotoImportRun{}, fmt.Errorf("starting photo import run: %w", err)
	}
	return run, nil
}

func (s *Store) PhotoImportRun(ctx context.Context, id string) (PhotoImportRun, error) {
	var run PhotoImportRun
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		run, err = s.photoImportRunTx(ctx, tx, id)
		return err
	})
	return run, err
}

// SetPhotoImportTotalGroups fills in the discovery count for a run created
// before the worker has walked the source tree.
func (s *Store) SetPhotoImportTotalGroups(ctx context.Context, id string, total int64) (PhotoImportRun, error) {
	if total < 0 {
		return PhotoImportRun{}, errors.New("photo import group count must not be negative")
	}
	var run PhotoImportRun
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := s.photoImportRunTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.CompletedGroups > total {
			return errors.New("photo import group count is below completed progress")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_import_runs SET total_groups=?,revision=revision+1,updated_at=? WHERE run_id=?`, total, nowRFC3339(), id); err != nil {
			return err
		}
		run, err = s.photoImportRunTx(ctx, tx, id)
		return err
	})
	return run, err
}

func (s *Store) ListPhotoImportRuns(ctx context.Context, limit int) ([]PhotoImportRun, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var runs []PhotoImportRun
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+photoImportRunColumns+` FROM photo_import_runs ORDER BY started_at DESC, run_id LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			run, scanErr := scanPhotoImportRun(rows)
			if scanErr != nil {
				return scanErr
			}
			runs = append(runs, run)
		}
		return rows.Err()
	})
	return runs, err
}

func (s *Store) RequestPhotoImportCancel(ctx context.Context, id string, revision int64) (PhotoImportRun, error) {
	var run PhotoImportRun
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := s.photoImportRunTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if revision > 0 && current.Revision != revision {
			return fmt.Errorf("photo import run at revision %d, expected %d: %w", current.Revision, revision, ErrStaleRevision)
		}
		if photoImportStateTerminal(current.State) {
			return fmt.Errorf("photo import run is terminal: %w", ErrStorageOperationTerminal)
		}
		nextState := current.State
		if nextState == PhotoImportStateRunning {
			nextState = PhotoImportStateCancelRequested
		}
		now := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `UPDATE photo_import_runs SET revision=revision+1,state=?,cancel_requested=1,updated_at=? WHERE run_id=?`, nextState, now, id); err != nil {
			return err
		}
		run, err = s.photoImportRunTx(ctx, tx, id)
		return err
	})
	return run, err
}

func (s *Store) UpdatePhotoImportProgress(ctx context.Context, id string, added, skipped, failed, ambiguous int64, ambiguity *PhotoImportAmbiguity) (PhotoImportRun, error) {
	var run PhotoImportRun
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := s.photoImportRunTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if photoImportStateTerminal(current.State) {
			return fmt.Errorf("photo import run is terminal: %w", ErrStorageOperationTerminal)
		}
		if err := updatePhotoImportRunTx(ctx, tx, id, added, skipped, failed, ambiguous, ambiguity); err != nil {
			return err
		}
		run, err = s.photoImportRunTx(ctx, tx, id)
		return err
	})
	return run, err
}

func updatePhotoImportRunTx(ctx context.Context, tx *sql.Tx, id string, added, skipped, failed, ambiguous int64, ambiguity *PhotoImportAmbiguity) error {
	if id == "" {
		return nil
	}
	current, err := scanPhotoImportRun(tx.QueryRowContext(ctx,
		`SELECT `+photoImportRunColumns+` FROM photo_import_runs WHERE run_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if photoImportStateTerminal(current.State) {
		return fmt.Errorf("photo import run is terminal: %w", ErrStorageOperationTerminal)
	}
	var ambiguityJSON any
	if ambiguity != nil {
		current.Ambiguities = append(current.Ambiguities, *ambiguity)
	}
	if len(current.Ambiguities) > 0 {
		ambiguityJSON, err = marshalPhotoImportAmbiguities(current.Ambiguities)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE photo_import_runs SET revision=revision+1,completed_groups=completed_groups+1,added_groups=added_groups+?,skipped_groups=skipped_groups+?,failed_groups=failed_groups+?,ambiguous_groups=ambiguous_groups+?,ambiguity_json=?,updated_at=? WHERE run_id=?`, added, skipped, failed, ambiguous, ambiguityJSON, nowRFC3339(), id)
	return err
}

func (s *Store) FinishPhotoImportRun(ctx context.Context, id, state, errorText string) (PhotoImportRun, error) {
	if !photoImportStateTerminal(state) {
		return PhotoImportRun{}, errors.New("photo import finish state must be terminal")
	}
	if len(errorText) > maxPhotoImportTextBytes {
		return PhotoImportRun{}, errors.New("photo import finish error is too large")
	}
	var run PhotoImportRun
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := s.photoImportRunTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if photoImportStateTerminal(current.State) {
			run = current
			return nil
		}
		now := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `UPDATE photo_import_runs SET revision=revision+1,state=?,error=?,finished_at=?,updated_at=? WHERE run_id=?`, state, nullablePhotoText(errorText), now, now, id); err != nil {
			return err
		}
		run, err = s.photoImportRunTx(ctx, tx, id)
		return err
	})
	return run, err
}

func (s *Store) MarkPhotoImportRunsInterrupted(ctx context.Context) error {
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		now := nowRFC3339()
		_, err := tx.ExecContext(ctx, `UPDATE photo_import_runs SET revision=revision+1,state=?,finished_at=?,updated_at=? WHERE state IN (?,?)`, PhotoImportStateInterrupted, now, now, PhotoImportStateRunning, PhotoImportStateCancelRequested)
		return err
	})
}
