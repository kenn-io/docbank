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

var errDocumentEventBuildInvalid = errors.New("stored document event rebuild is invalid")

var documentEventBuilds = rebuildReceiptTable{
	table: "document_event_builds", label: "document event rebuild",
	conflict: ErrDocumentEventBuildConflict, invalid: errDocumentEventBuildInvalid,
	selectSQL: `SELECT operation_id,request_sha256,deriver_fingerprint,
		state,target_epoch,scanned,published,failed,unavailable,started_at,updated_at,finished_at
		FROM document_event_builds WHERE operation_id=?`,
	insertSQL: `INSERT INTO document_event_builds(
		operation_id,request_sha256,deriver_fingerprint,state,target_epoch,started_at,updated_at,
		scanned,published,failed,unavailable,finished_at
	) VALUES(?,?,?,?,?,?,?,0,0,0,0,NULL)`,
	updateSQL: `UPDATE document_event_builds SET
		state=?,scanned=?,published=?,failed=?,unavailable=?,updated_at=?,finished_at=?
		WHERE operation_id=? AND state='running'`,
	count: func(ctx context.Context, q metadataQuerier, b rebuildReceipt) (rebuildCounts, error) {
		var c rebuildCounts
		err := q.QueryRowContext(ctx, `SELECT count(*),
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
			b.TargetEpoch, b.TargetEpoch, b.TargetEpoch).Scan(&c.selected, &c.published, &c.failed, &c.unavailable)
		if err != nil {
			return rebuildCounts{}, fmt.Errorf("counting document event rebuild progress: %w", err)
		}
		return c, nil
	},
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
	b, err := documentEventBuilds.start(ctx, s, operationID, requestSHA256, DocumentEventsDeriverFingerprint,
		func(tx *sql.Tx) (int64, error) {
			return bumpDocumentEventInputEpochTx(ctx, tx, DocumentEventsDeriverFingerprint)
		})
	return DocumentEventBuild(b), err
}

// DocumentEventBuild returns one rebuild receipt by operation ID.
func (s *Store) DocumentEventBuild(
	ctx context.Context,
	operationID string,
) (DocumentEventBuild, error) {
	b, err := documentEventBuilds.get(ctx, s, operationID)
	return DocumentEventBuild(b), err
}

// RefreshDocumentEventBuilds updates running receipts from fenced terminal
// attempts. A build remains running while any retained target lacks a current
// attempt at its epoch or a newer one.
func (s *Store) RefreshDocumentEventBuilds(ctx context.Context) error {
	return documentEventBuilds.refresh(ctx, s)
}
