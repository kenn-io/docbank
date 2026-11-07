package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type NotePublicationAcceptLocalRequest struct {
	Name, DatabaseEpoch, SourceRef, ExpectedHeadVersionID, OperationID string
	ExpectedSourceRevision                                             int64
}

// AcceptLocalPublishedNote grants one verified upload against the exact archive
// head reviewed by the operator. It never adopts that head or changes content.
func (s *Store) AcceptLocalPublishedNote(ctx context.Context, request NotePublicationAcceptLocalRequest) (NotePublicationRecoveryGrant, error) {
	var grant NotePublicationRecoveryGrant
	if err := ValidatePushName(request.Name); err != nil {
		return grant, err
	}
	if err := ValidatePushRef(request.SourceRef); err != nil {
		return grant, err
	}
	if err := validateUUIDv4(request.ExpectedHeadVersionID); err != nil {
		return grant, err
	}
	if err := validateUUIDv4(request.OperationID); err != nil {
		return grant, err
	}
	if err := validateUUIDv4(request.DatabaseEpoch); err != nil {
		return grant, err
	}
	if request.ExpectedSourceRevision <= 0 {
		return grant, errors.New("invalid expected source revision")
	}
	digest := publicationRequestDigest(request)
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var databaseEpoch string
		if err := tx.QueryRowContext(ctx, `SELECT database_epoch FROM note_publication_state WHERE singleton=1`).Scan(&databaseEpoch); err != nil {
			return err
		}
		if databaseEpoch != request.DatabaseEpoch {
			return ErrPublicationScanSuperseded
		}
		var previous, encoded string
		replayErr := tx.QueryRowContext(ctx, `SELECT request_sha256,grant_json FROM note_publication_recovery_operations WHERE operation_id=$1`, request.OperationID).Scan(&previous, &encoded)
		if replayErr != nil && !errors.Is(replayErr, sql.ErrNoRows) {
			return replayErr
		}
		if replayErr == nil && previous != digest {
			return ErrPublicationOperationMismatch
		}
		source, err := s.publishedNoteSourceTx(ctx, tx, request.Name, request.SourceRef)
		if err != nil {
			return err
		}
		if source.Identity.SourceRevision != request.ExpectedSourceRevision {
			return ErrPublicationSourceStale
		}
		if source.Availability != "retained" || source.ArchiveHead == nil {
			return ErrNoteSourceUnavailable
		}
		if source.ArchiveHead.ContentVersionID != request.ExpectedHeadVersionID || source.Conflict != "diverged" {
			return ErrPublishedSourceDiverged
		}
		if replayErr == nil {
			if source.Recovery == nil {
				return ErrPublishedSourceDiverged
			}
			return json.Unmarshal([]byte(encoded), &grant)
		}
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM note_publication_operations WHERE operation_id=$1)+(SELECT COUNT(*) FROM note_publication_scans WHERE begin_operation_id=$1)`, request.OperationID).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return ErrPublicationOperationMismatch
		}
		grant = NotePublicationRecoveryGrant{SourceRef: request.SourceRef, ExpectedHeadVersionID: request.ExpectedHeadVersionID, SourceRevision: request.ExpectedSourceRevision}
		_, err = tx.ExecContext(ctx, `INSERT INTO note_publication_recoveries(publication_name,source_ref,head_version_id,source_revision) VALUES($1,$2,$3,$4)
		 ON CONFLICT(publication_name,source_ref) DO UPDATE SET head_version_id=excluded.head_version_id,source_revision=excluded.source_revision`, request.Name, request.SourceRef, request.ExpectedHeadVersionID, request.ExpectedSourceRevision)
		if err != nil {
			return err
		}
		data, err := json.Marshal(grant)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO note_publication_recovery_operations(operation_id,request_sha256,grant_json) VALUES($1,$2,$3)`, request.OperationID, digest, string(data))
		return err
	})
	return grant, err
}

// invalidateNotePublicationsTx is shared by lifecycle change recording, including
// audited paths. Subtree membership is resolved in SQL, without a vault ID list.
func invalidateNotePublicationsTx(ctx context.Context, tx *sql.Tx, nodeIDs []int64) error {
	for start := 0; start < len(nodeIDs); start += 400 {
		end := min(start+400, len(nodeIDs))
		args := make([]any, end-start)
		for i, id := range nodeIDs[start:end] {
			args[i] = id
		}
		ids := numberedPlaceholders(len(args), 0)
		cte := `WITH RECURSIVE affected(id) AS (SELECT id FROM nodes WHERE id IN (` + ids + `) UNION SELECT n.id FROM nodes n JOIN affected a ON n.parent_id=a.id) `
		rows, err := tx.QueryContext(ctx, cte+`SELECT DISTINCT publication_name FROM published_note_sources WHERE node_id IN (SELECT id FROM affected)`, args...)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return err
			}
			names = append(names, name)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := bumpNotePublicationTx(ctx, tx, name); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, cte+`DELETE FROM note_publication_recoveries WHERE (publication_name,source_ref) IN (SELECT s.publication_name,s.source_ref FROM published_note_sources s
            JOIN nodes n ON n.id=s.node_id
            JOIN note_publication_recoveries g ON g.publication_name=s.publication_name AND g.source_ref=s.source_ref
            WHERE s.node_id IN (SELECT id FROM affected) AND
            (n.current_version_id<>g.head_version_id OR n.trashed_at IS NOT NULL OR s.source_revision<>g.source_revision OR s.availability<>'retained'))`, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

func invalidatePrunedNoteVersionsTx(ctx context.Context, tx *sql.Tx, versions []string) error {
	for _, version := range versions {
		var name, ref string
		err := tx.QueryRowContext(ctx, `SELECT publication_name,source_ref FROM published_note_sources WHERE content_version_id=$1 AND availability='retained'`, version).Scan(&name, &ref)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE published_note_sources SET availability='unavailable' WHERE publication_name=$1 AND source_ref=$2`, name, ref); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM note_publication_recoveries WHERE publication_name=$1 AND source_ref=$2`, name, ref); err != nil {
			return err
		}
		if err := bumpNotePublicationTx(ctx, tx, name); err != nil {
			return fmt.Errorf("invalidating pruned published source: %w", err)
		}
	}
	return nil
}
