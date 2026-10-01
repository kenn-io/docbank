package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

// DocumentPeopleBuild is one durable, idempotent full-rebuild receipt.
type DocumentPeopleBuild struct {
	OperationID, RequestSHA256, ResolverFingerprint, State string
	TargetEpoch, Scanned, Published, Failed                int64
	StartedAt, UpdatedAt, FinishedAt                       string
}

// DocumentPeopleCoverage reports current-file attribution coverage and the
// bounded authority that still needs operator attention.
type DocumentPeopleCoverage struct {
	ContractVersion, ResolverFingerprint                                     string
	BindingEpoch, PublicationEpoch                                           int64
	Published, Pending, Failed, Unavailable                                  int64
	UnresolvedActors, SuppressedActors, UnresolvedCustodians, OpenCandidates int64
	CandidateQueueFull                                                       bool
}

func documentPeopleRebuildDigest(fingerprint string) string {
	digest := sha256.Sum256([]byte("people-rebuild\x00" + fingerprint))
	return hex.EncodeToString(digest[:])
}

var errDocumentPeopleBuildConflict = fmt.Errorf("document people rebuild operation conflicts: %w", ErrExists)

var documentPeopleBuilds = rebuildReceiptTable{
	table: "document_people_builds", label: "document people rebuild",
	conflict: errDocumentPeopleBuildConflict, invalid: ErrDocumentPeopleCorrupt,
	selectSQL: `SELECT operation_id,request_sha256,resolver_fingerprint,state,target_epoch,scanned,published,failed,0,started_at,updated_at,NULLIF(finished_at,'') FROM document_people_builds WHERE operation_id=?`,
	insertSQL: `INSERT INTO document_people_builds(operation_id,request_sha256,resolver_fingerprint,state,target_epoch,started_at,updated_at,scanned,published,failed,finished_at) VALUES(?,?,?,?,?,?,?,0,0,0,'')`,
	// People count unavailable heads inside failed, so the added unavailable bind is always 0.
	updateSQL: `UPDATE document_people_builds SET state=?,scanned=?,published=?,failed=?+?,updated_at=?,finished_at=COALESCE(?,'') WHERE operation_id=? AND state='running'`,
	count: func(ctx context.Context, q metadataQuerier, b rebuildReceipt) (rebuildCounts, error) {
		var c rebuildCounts
		err := q.QueryRowContext(ctx, `SELECT count(*),
			COALESCE(sum(CASE WHEN ph.node_revision=n.revision AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch>=? AND ph.resolver_fingerprint=? AND ph.state='published' THEN 1 ELSE 0 END),0),
			COALESCE(sum(CASE WHEN ph.node_revision=n.revision AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch>=? AND ph.resolver_fingerprint=? AND ph.state IN ('failed','unavailable') THEN 1 ELSE 0 END),0),
			COALESCE(sum(CASE WHEN ph.node_revision=n.revision AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch>=? AND ph.resolver_fingerprint=? AND ph.state='failed' THEN 1 ELSE 0 END),0)
			FROM content_versions cv JOIN nodes n ON n.id=cv.node_id LEFT JOIN document_event_heads eh ON eh.content_version_id=cv.version_id
			LEFT JOIN document_people_heads ph ON ph.content_version_id=cv.version_id`,
			b.TargetEpoch, b.DeriverFingerprint, b.TargetEpoch, b.DeriverFingerprint,
			b.TargetEpoch, b.DeriverFingerprint).Scan(&c.selected, &c.published, &c.failed, &c.retrying)
		return c, err
	},
}

// RebuildDocumentPeople creates or replays a rebuild receipt and fences all
// prior publications by advancing the binding epoch in the same transaction.
func (s *Store) RebuildDocumentPeople(ctx context.Context, operationID string) (DocumentPeopleBuild, error) {
	if validateUUIDv4(operationID) != nil {
		return DocumentPeopleBuild{}, errors.New("document people rebuild operation ID must be a UUID")
	}
	fingerprint := document.PersonResolverFingerprint()
	b, err := documentPeopleBuilds.start(ctx, s, operationID, documentPeopleRebuildDigest(fingerprint), fingerprint,
		func(tx *sql.Tx) (epoch int64, err error) {
			if _, err := tx.ExecContext(ctx, `UPDATE document_people_state SET binding_epoch=binding_epoch+1,resolver_fingerprint=?,updated_at=? WHERE singleton=1`, fingerprint, nowRFC3339()); err != nil {
				return 0, fmt.Errorf("advancing document people rebuild epoch: %w", err)
			}
			if err := tx.QueryRowContext(ctx, `SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch); err != nil {
				return 0, fmt.Errorf("reading document people rebuild epoch: %w", err)
			}
			return epoch, nil
		})
	return b.peopleBuild(), err
}

func (s *Store) DocumentPeopleBuild(ctx context.Context, operationID string) (DocumentPeopleBuild, error) {
	b, err := documentPeopleBuilds.get(ctx, s, operationID)
	return b.peopleBuild(), err
}

// RefreshDocumentPeopleBuilds records current progress for every running
// rebuild. Failed heads stay running because the worker retries them; an
// unavailable head is terminal and makes the completed receipt failed.
func (s *Store) RefreshDocumentPeopleBuilds(ctx context.Context) error {
	return documentPeopleBuilds.refresh(ctx, s)
}

func (b rebuildReceipt) peopleBuild() DocumentPeopleBuild {
	finished := ""
	if b.FinishedAt != nil {
		finished = *b.FinishedAt
	}
	return DocumentPeopleBuild{
		OperationID: b.OperationID, RequestSHA256: b.RequestSHA256, ResolverFingerprint: b.DeriverFingerprint,
		State: b.State, TargetEpoch: b.TargetEpoch, Scanned: b.Scanned, Published: b.Published, Failed: b.Failed,
		StartedAt: b.StartedAt, UpdatedAt: b.UpdatedAt, FinishedAt: finished,
	}
}

func (s *Store) DocumentPeopleCoverage(ctx context.Context) (DocumentPeopleCoverage, error) {
	coverage := DocumentPeopleCoverage{
		ContractVersion: document.PersonContractV1, ResolverFingerprint: document.PersonResolverFingerprint(),
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return coverage, fmt.Errorf("starting document people coverage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRowContext(ctx, `SELECT binding_epoch,publication_epoch FROM document_people_state WHERE singleton=1`).Scan(
		&coverage.BindingEpoch, &coverage.PublicationEpoch)
	if err != nil {
		return coverage, err
	}
	var selected int64
	err = tx.QueryRowContext(ctx, `SELECT count(*),
		COALESCE(sum(CASE WHEN ph.node_revision=n.revision AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? AND ph.state='published' THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN ph.node_revision=n.revision AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? AND ph.state='failed' THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN ph.node_revision=n.revision AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? AND ph.state='unavailable' THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN ph.node_revision=n.revision AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? THEN ph.unresolved_actors ELSE 0 END),0),
		COALESCE(sum(CASE WHEN ph.node_revision=n.revision AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? THEN ph.suppressed_actors ELSE 0 END),0)
		FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id LEFT JOIN document_event_heads eh ON eh.content_version_id=cv.version_id
		LEFT JOIN document_people_heads ph ON ph.content_version_id=cv.version_id WHERE n.kind='file'`,
		coverage.BindingEpoch, coverage.ResolverFingerprint, coverage.BindingEpoch, coverage.ResolverFingerprint,
		coverage.BindingEpoch, coverage.ResolverFingerprint, coverage.BindingEpoch, coverage.ResolverFingerprint,
		coverage.BindingEpoch, coverage.ResolverFingerprint).Scan(&selected, &coverage.Published, &coverage.Failed,
		&coverage.Unavailable, &coverage.UnresolvedActors, &coverage.SuppressedActors)
	if err != nil {
		return coverage, err
	}
	coverage.Pending = selected - coverage.Published - coverage.Failed - coverage.Unavailable
	if coverage.Pending < 0 {
		return coverage, ErrDocumentPeopleCorrupt
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM custodian_assignments ca LEFT JOIN persons p ON p.person_id=ca.person_id WHERE ca.retired_at IS NULL AND (ca.person_id IS NULL OR p.person_id IS NULL OR p.state='retired')`).Scan(&coverage.UnresolvedCustodians)
	if err != nil {
		return coverage, err
	}
	coverage.OpenCandidates, coverage.CandidateQueueFull, err = openPersonCandidateCount(ctx, tx)
	if err != nil {
		return coverage, err
	}
	if err := tx.Commit(); err != nil {
		return DocumentPeopleCoverage{}, fmt.Errorf("closing document people coverage: %w", err)
	}
	return coverage, nil
}
