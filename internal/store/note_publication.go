package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/packstore"
)

const notePublicationsStorageSchemaVersion = 49

// PublishedNoteSelector limits a query to current, eligible authored sources.
// An empty publication selects all active publications. A nil selector leaves
// the ordinary archive view unchanged.
type PublishedNoteSelector struct{ Publication string }

// PublishedNoteIdentity binds an authored source to its accepted archive version.
type PublishedNoteIdentity struct {
	VaultID, Publication, SourceRef         string
	NodeID                                  int64
	ContentVersionID, Hash                  string
	Size, SourceRevision, NamespaceRevision int64
}

type NoteArchiveHead struct {
	NodeID                 int64
	ContentVersionID, Hash string
	Size                   int64
}

type NotePublicationScan struct {
	ScanID, DatabaseEpoch, PolicyFingerprint string
	Epoch, NamespaceRevision                 int64
}

type NotePublicationRecoveryGrant struct {
	SourceRef, ExpectedHeadVersionID string
	SourceRevision                   int64
}

// NotePublicationSource keeps source presence separate from archive liveness.
// Unavailable observations survive pruning without retaining the original blob.
type NotePublicationSource struct {
	Identity                         PublishedNoteIdentity
	ArchiveHead                      *NoteArchiveHead
	Presence, Availability, Conflict string
	Recovery                         *NotePublicationRecoveryGrant
}

type NotePublicationStatus struct {
	Name, DatabaseEpoch, LastCompleteAt, LastAcceptedAt               string
	NamespaceRevision                                                 int64
	ActiveScan                                                        *NotePublicationScan
	Incomplete                                                        bool
	Present, Absent, Unknown, Conflicts, Unavailable, RecoveryPending int64
}

// ValidatePublishedNoteSource validates the portable observation, independently
// of the retained archive. Import and audit additionally check its relationships.
func ValidatePublishedNoteSource(source NotePublicationSource) error {
	id := source.Identity
	if err := validateUUIDv4(id.VaultID); err != nil {
		return fmt.Errorf("published note vault: %w", err)
	}
	if err := ValidatePushName(id.Publication); err != nil {
		return err
	}
	if err := ValidatePushRef(id.SourceRef); err != nil {
		return err
	}
	if id.NodeID <= 0 || id.Size < 0 || id.SourceRevision <= 0 || id.NamespaceRevision <= 0 {
		return errors.New("invalid published note identity or revision")
	}
	if err := validateUUIDv4(id.ContentVersionID); err != nil {
		return fmt.Errorf("published note version: %w", err)
	}
	if _, err := packstore.ParseHash(id.Hash); err != nil {
		return fmt.Errorf("published note hash: %w", err)
	}
	switch source.Presence {
	case "present", "absent", "unknown":
	default:
		return errors.New("invalid published note presence")
	}
	switch source.Availability {
	case "retained", "unavailable":
	default:
		return errors.New("invalid published note availability")
	}
	return nil
}

// NotePublicationStatus returns one snapshot. After restore the preserved
// membership has incomplete freshness until a new inventory completes.
func (s *Store) NotePublicationStatus(ctx context.Context, name string) (NotePublicationStatus, error) {
	if err := ValidatePushName(name); err != nil {
		return NotePublicationStatus{}, err
	}
	var status NotePublicationStatus
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		status, err = notePublicationStatusTx(ctx, tx, name)
		return err
	})
	return status, err
}

func notePublicationStatusTx(ctx context.Context, q metadataQuerier, name string) (NotePublicationStatus, error) {
	status := NotePublicationStatus{Name: name, Incomplete: true}
	err := q.QueryRowContext(ctx, `SELECT p.namespace_revision,p.last_complete_at,p.last_accepted_at,r.database_epoch
		FROM note_publications p CROSS JOIN note_publication_state r WHERE p.name=$1 AND r.singleton=1`, name).
		Scan(&status.NamespaceRevision, &status.LastCompleteAt, &status.LastAcceptedAt, &status.DatabaseEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return status, ErrNotFound
	}
	if err != nil {
		return status, err
	}
	var scan NotePublicationScan
	var sealed bool
	err = q.QueryRowContext(ctx, `SELECT scan_id,database_epoch,policy_fingerprint,epoch,namespace_revision,sealed
		FROM note_publication_scans WHERE publication_name=$1 AND database_epoch=$2 ORDER BY epoch DESC LIMIT 1`, name, status.DatabaseEpoch).
		Scan(&scan.ScanID, &scan.DatabaseEpoch, &scan.PolicyFingerprint, &scan.Epoch, &scan.NamespaceRevision, &sealed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return status, err
	}
	if err == nil {
		status.Incomplete = !sealed
		if !sealed {
			status.ActiveScan = &scan
		}
	}
	err = q.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN s.presence='present' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN s.presence='absent' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN s.presence='unknown' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN n.id IS NOT NULL AND n.current_version_id<>s.content_version_id THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN s.availability='unavailable' OR v.version_id IS NULL OR n.id IS NULL THEN 1 ELSE 0 END),0)
		FROM published_note_sources s LEFT JOIN nodes n ON n.id=s.node_id
		LEFT JOIN content_versions v ON v.version_id=s.content_version_id WHERE s.publication_name=$1`, name).
		Scan(&status.Present, &status.Absent, &status.Unknown, &status.Conflicts, &status.Unavailable)
	if err == nil {
		err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_publication_recoveries g
        JOIN published_note_sources s ON s.publication_name=g.publication_name AND s.source_ref=g.source_ref
        JOIN nodes n ON n.id=s.node_id
        WHERE g.publication_name=$1 AND g.head_version_id=n.current_version_id AND g.source_revision=s.source_revision
        AND s.availability='retained' AND n.trashed_at IS NULL`, name).Scan(&status.RecoveryPending)
	}
	return status, err
}

func initializeNotePublicationState(ctx context.Context, tx *sql.Tx, replace bool) error {
	epoch, err := newUUIDv4()
	if err != nil {
		return err
	}
	query := `INSERT INTO note_publication_state(singleton,database_epoch) VALUES(1,$1) ON CONFLICT(singleton) DO NOTHING`
	if replace {
		query = `UPDATE note_publication_state SET database_epoch=$1 WHERE singleton=1`
	}
	_, err = tx.ExecContext(ctx, query, epoch)
	return err
}
