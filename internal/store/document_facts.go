package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DocumentDateFacts is a version and its current derived events, read together.
type DocumentDateFacts struct {
	Version ContentVersion
	State   string
	View    DocumentEventView
}

func (s *Store) DocumentDateFacts(ctx context.Context, versionID string) (DocumentDateFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return DocumentDateFacts{}, err
	}
	defer func() { _ = tx.Rollback() }()
	version, err := scanContentVersion(tx.QueryRowContext(ctx, `SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=$1`, versionID))
	if err != nil {
		return DocumentDateFacts{}, err
	}
	state, err := effectiveDocumentEventState(ctx, tx, DocumentEventsDeriverFingerprint)
	if err != nil {
		return DocumentDateFacts{}, err
	}
	var attempt, head, fresh, failed bool
	err = tx.QueryRowContext(ctx, `SELECT a.content_version_id IS NOT NULL,h.content_version_id IS NOT NULL,
 COALESCE(a.input_epoch=$2 AND a.input_revision=COALESCE(d.revision,0) AND a.state='indexed' AND h.input_epoch=a.input_epoch AND g.inputs_sha256=a.inputs_sha256 AND g.contract_version=$3 AND g.deriver_fingerprint=$4,false),
 COALESCE(a.input_epoch=$2 AND a.input_revision=COALESCE(d.revision,0) AND a.state IN ('failed','unavailable'),false)
 FROM content_versions cv LEFT JOIN document_event_dirty d ON d.content_version_id=cv.version_id
 LEFT JOIN document_event_attempts a ON a.content_version_id=cv.version_id
 LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id
 LEFT JOIN document_event_generations g ON g.generation_id=h.generation_id WHERE cv.version_id=$1`, versionID, state.InputEpoch, state.ContractVersion, state.DeriverFingerprint).Scan(&attempt, &head, &fresh, &failed)
	if err != nil {
		return DocumentDateFacts{}, fmt.Errorf("reading date state: %w", err)
	}
	facts := DocumentDateFacts{Version: version, State: "pending"}
	switch {
	case fresh:
		facts.State = "published"
		facts.View, err = s.documentEventsForVersionTx(ctx, tx, versionID)
		if err != nil {
			return DocumentDateFacts{}, err
		}
	case failed:
		facts.State = "failed"
	case !attempt && !head:
		facts.State = "none"
	}
	if err := tx.Commit(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return DocumentDateFacts{}, err
	}
	return facts, nil
}
