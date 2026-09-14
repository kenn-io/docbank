package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrDocumentEventBuildConflict means an operation ID was replayed with a
// different request identity.
var ErrDocumentEventBuildConflict = errors.New("document event rebuild operation conflicts with its original request")

// DocumentEventRebuildRequestSHA256 identifies the empty semantic payload
// shared by daemon and embedded full-rebuild requests.
const DocumentEventRebuildRequestSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// DocumentEventBuild is one durable, idempotent full-rebuild receipt.
type DocumentEventBuild struct {
	OperationID        string
	RequestSHA256      string
	DeriverFingerprint string
	State              string
	TargetEpoch        int64
	Scanned            int64
	Published          int64
	Failed             int64
	Unavailable        int64
	StartedAt          string
	UpdatedAt          string
	FinishedAt         *string
}

// StartDocumentEventRebuild creates or replays one rebuild receipt. Recipe
// installation, the epoch bump, and receipt insertion share one transaction.
func (s *Store) StartDocumentEventRebuild(
	ctx context.Context,
	operationID, requestSHA256 string,
) (DocumentEventBuild, error) {
	if err := validateUUIDv4(operationID); err != nil {
		return DocumentEventBuild{}, errors.New("document event rebuild operation ID must be a UUID")
	}
	if err := validateCatalogSHA256(requestSHA256, "document event rebuild request digest"); err != nil {
		return DocumentEventBuild{}, err
	}
	var build DocumentEventBuild
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, err := readDocumentEventBuild(ctx, tx, operationID)
		if err == nil {
			if stored.RequestSHA256 != requestSHA256 {
				return ErrDocumentEventBuildConflict
			}
			build = stored
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		epoch, err := bumpDocumentEventInputEpochTx(ctx, tx, DocumentEventsDeriverFingerprint)
		if err != nil {
			return err
		}
		now := nowRFC3339()
		build = DocumentEventBuild{
			OperationID: operationID, RequestSHA256: requestSHA256,
			DeriverFingerprint: DocumentEventsDeriverFingerprint,
			State:              "running", TargetEpoch: epoch, StartedAt: now, UpdatedAt: now,
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO document_event_builds(
			operation_id,request_sha256,deriver_fingerprint,state,target_epoch,
			scanned,published,failed,unavailable,started_at,updated_at,finished_at
		) VALUES(?,?,?,?,?,0,0,0,0,?,?,NULL)`, build.OperationID, build.RequestSHA256,
			build.DeriverFingerprint, build.State, build.TargetEpoch, build.StartedAt,
			build.UpdatedAt)
		if err != nil {
			return fmt.Errorf("recording document event rebuild: %w", err)
		}
		return nil
	})
	return build, err
}

// DocumentEventBuild returns one rebuild receipt by operation ID.
func (s *Store) DocumentEventBuild(
	ctx context.Context,
	operationID string,
) (DocumentEventBuild, error) {
	if err := validateUUIDv4(operationID); err != nil {
		return DocumentEventBuild{}, fmt.Errorf("document event rebuild %q: %w", operationID, ErrNotFound)
	}
	return readDocumentEventBuild(ctx, s.db, operationID)
}

func readDocumentEventBuild(
	ctx context.Context,
	q metadataQuerier,
	operationID string,
) (DocumentEventBuild, error) {
	var build DocumentEventBuild
	var finished sql.NullString
	err := q.QueryRowContext(ctx, `SELECT operation_id,request_sha256,deriver_fingerprint,
		state,target_epoch,scanned,published,failed,unavailable,started_at,updated_at,finished_at
		FROM document_event_builds WHERE operation_id=?`, operationID).Scan(
		&build.OperationID, &build.RequestSHA256, &build.DeriverFingerprint, &build.State,
		&build.TargetEpoch, &build.Scanned, &build.Published, &build.Failed,
		&build.Unavailable, &build.StartedAt, &build.UpdatedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentEventBuild{}, fmt.Errorf("document event rebuild %q: %w", operationID, ErrNotFound)
	}
	if err != nil {
		return DocumentEventBuild{}, fmt.Errorf("reading document event rebuild: %w", err)
	}
	if finished.Valid {
		build.FinishedAt = new(finished.String)
	}
	if err := validateDocumentEventBuild(build); err != nil {
		return DocumentEventBuild{}, err
	}
	return build, nil
}

func validateDocumentEventBuild(build DocumentEventBuild) error {
	if err := validateUUIDv4(build.OperationID); err != nil {
		return fmt.Errorf("invalid stored document event rebuild operation ID: %w", err)
	}
	if err := validateCatalogSHA256(build.RequestSHA256, "stored document event rebuild request digest"); err != nil {
		return err
	}
	if err := validateCatalogSHA256(build.DeriverFingerprint, "stored document event rebuild fingerprint"); err != nil {
		return err
	}
	if build.TargetEpoch <= 0 || build.Scanned < 0 || build.Published < 0 ||
		build.Failed < 0 || build.Unavailable < 0 ||
		build.Scanned != build.Published+build.Failed+build.Unavailable {
		return errors.New("stored document event rebuild counters are invalid")
	}
	switch build.State {
	case "running":
		if build.FinishedAt != nil {
			return errors.New("running document event rebuild has a finish time")
		}
	case "completed", "failed":
		if build.FinishedAt == nil {
			return errors.New("terminal document event rebuild lacks a finish time")
		}
	default:
		return fmt.Errorf("stored document event rebuild state %q is invalid", build.State)
	}
	return nil
}

// RefreshDocumentEventBuilds updates running receipts from fenced terminal
// attempts. A build remains running while any retained target lacks a current
// attempt at its epoch or a newer one.
func (s *Store) RefreshDocumentEventBuilds(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id FROM document_event_builds
		WHERE state='running' ORDER BY started_at,operation_id`)
	if err != nil {
		return fmt.Errorf("listing running document event rebuilds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var operationIDs []string
	for rows.Next() {
		var operationID string
		if err := rows.Scan(&operationID); err != nil {
			return fmt.Errorf("scanning running document event rebuild: %w", err)
		}
		operationIDs = append(operationIDs, operationID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing running document event rebuilds: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing running document event rebuilds: %w", err)
	}
	for _, operationID := range operationIDs {
		if _, changed, err := documentEventBuildProgress(ctx, s.db, operationID); err != nil {
			return err
		} else if !changed {
			continue
		}
		// Recheck after acquiring the writer so concurrent publications and
		// refreshes cannot overwrite newer receipt progress.
		if err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
			build, changed, err := documentEventBuildProgress(ctx, tx, operationID)
			if err != nil || !changed {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE document_event_builds SET
				state=?,scanned=?,published=?,failed=?,unavailable=?,updated_at=?,finished_at=?
				WHERE operation_id=? AND state='running'`, build.State, build.Scanned,
				build.Published, build.Failed, build.Unavailable, build.UpdatedAt,
				build.FinishedAt, operationID)
			if err != nil {
				return fmt.Errorf("refreshing document event rebuild: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func documentEventBuildProgress(
	ctx context.Context, q metadataQuerier, operationID string,
) (DocumentEventBuild, bool, error) {
	build, err := readDocumentEventBuild(ctx, q, operationID)
	if err != nil || build.State != "running" {
		return build, false, err
	}
	var selected, scanned, published, failed, unavailable int64
	err = q.QueryRowContext(ctx, `SELECT count(*),
		COALESCE(sum(CASE WHEN a.input_epoch>=? AND
			a.input_revision=COALESCE(d.revision,0) AND
			(a.state IN ('failed','unavailable') OR
			 (a.state='indexed' AND h.input_epoch=a.input_epoch AND
			  g.inputs_sha256=a.inputs_sha256)) THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN a.input_epoch>=? AND
			a.input_revision=COALESCE(d.revision,0) AND a.state='indexed' AND
			h.input_epoch=a.input_epoch AND g.inputs_sha256=a.inputs_sha256
			THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN a.input_epoch>=? AND
			a.input_revision=COALESCE(d.revision,0) AND a.state='failed'
			THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN a.input_epoch>=? AND
			a.input_revision=COALESCE(d.revision,0) AND a.state='unavailable'
			THEN 1 ELSE 0 END),0)
		FROM content_versions cv
		LEFT JOIN document_event_dirty d ON d.content_version_id=cv.version_id
		LEFT JOIN document_event_attempts a ON a.content_version_id=cv.version_id
		LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id
		LEFT JOIN document_event_generations g ON g.generation_id=h.generation_id`,
		build.TargetEpoch, build.TargetEpoch, build.TargetEpoch, build.TargetEpoch).
		Scan(&selected, &scanned, &published, &failed, &unavailable)
	if err != nil {
		return DocumentEventBuild{}, false, fmt.Errorf("counting document event rebuild progress: %w", err)
	}
	state := "running"
	if scanned == selected {
		state = "completed"
		if failed+unavailable > 0 {
			state = "failed"
		}
	}
	if build.Scanned == scanned && build.Published == published &&
		build.Failed == failed && build.Unavailable == unavailable && state == "running" {
		return build, false, nil
	}
	build.State, build.Scanned, build.Published = state, scanned, published
	build.Failed, build.Unavailable = failed, unavailable
	build.UpdatedAt = nowRFC3339()
	if state != "running" {
		build.FinishedAt = new(build.UpdatedAt)
	}
	return build, true, nil
}
