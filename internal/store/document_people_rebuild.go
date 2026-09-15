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

const (
	documentPeopleBuildRunning = string(RenditionJobRunning)
	documentPeopleBuildFailed  = string(ExtractionFailed)
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

// RebuildDocumentPeople creates or replays a rebuild receipt and fences all
// prior publications by advancing the binding epoch in the same transaction.
func (s *Store) RebuildDocumentPeople(ctx context.Context, operationID string) (DocumentPeopleBuild, error) {
	if validateUUIDv4(operationID) != nil {
		return DocumentPeopleBuild{}, errors.New("document people rebuild operation ID must be a UUID")
	}
	fingerprint := document.PersonResolverFingerprint()
	requestSHA256 := documentPeopleRebuildDigest(fingerprint)
	var build DocumentPeopleBuild
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, err := readDocumentPeopleBuild(ctx, tx, operationID)
		if err == nil {
			if stored.RequestSHA256 != requestSHA256 || stored.ResolverFingerprint != fingerprint {
				return fmt.Errorf("document people rebuild operation conflicts: %w", ErrExists)
			}
			build = stored
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		now := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `UPDATE document_people_state SET binding_epoch=binding_epoch+1,resolver_fingerprint=?,updated_at=? WHERE singleton=1`, fingerprint, now); err != nil {
			return fmt.Errorf("advancing document people rebuild epoch: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&build.TargetEpoch); err != nil {
			return fmt.Errorf("reading document people rebuild epoch: %w", err)
		}
		build.OperationID, build.RequestSHA256 = operationID, requestSHA256
		build.ResolverFingerprint, build.State = fingerprint, documentPeopleBuildRunning
		build.StartedAt, build.UpdatedAt = now, now
		_, err = tx.ExecContext(ctx, `INSERT INTO document_people_builds(operation_id,request_sha256,resolver_fingerprint,target_epoch,state,scanned,published,failed,started_at,updated_at,finished_at) VALUES(?,?,?,?,?,0,0,0,?,?,'')`,
			operationID, requestSHA256, fingerprint, build.TargetEpoch, build.State, now, now)
		if err != nil {
			return fmt.Errorf("recording document people rebuild: %w", err)
		}
		return nil
	})
	return build, err
}

func (s *Store) DocumentPeopleBuild(ctx context.Context, operationID string) (DocumentPeopleBuild, error) {
	if validateUUIDv4(operationID) != nil {
		return DocumentPeopleBuild{}, fmt.Errorf("document people rebuild %q: %w", operationID, ErrNotFound)
	}
	return readDocumentPeopleBuild(ctx, s.db, operationID)
}

func readDocumentPeopleBuild(ctx context.Context, q metadataQuerier, operationID string) (DocumentPeopleBuild, error) {
	var build DocumentPeopleBuild
	err := q.QueryRowContext(ctx, `SELECT operation_id,request_sha256,resolver_fingerprint,state,target_epoch,scanned,published,failed,started_at,updated_at,finished_at FROM document_people_builds WHERE operation_id=?`, operationID).Scan(
		&build.OperationID, &build.RequestSHA256, &build.ResolverFingerprint, &build.State,
		&build.TargetEpoch, &build.Scanned, &build.Published, &build.Failed,
		&build.StartedAt, &build.UpdatedAt, &build.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentPeopleBuild{}, fmt.Errorf("document people rebuild %q: %w", operationID, ErrNotFound)
	}
	if err != nil {
		return DocumentPeopleBuild{}, fmt.Errorf("reading document people rebuild: %w", err)
	}
	if validateUUIDv4(build.OperationID) != nil || validateCatalogSHA256(build.RequestSHA256, "stored document people rebuild request digest") != nil || validateCatalogSHA256(build.ResolverFingerprint, "stored document people rebuild fingerprint") != nil {
		return DocumentPeopleBuild{}, ErrDocumentPeopleCorrupt
	}
	if build.TargetEpoch < 1 || build.Scanned < 0 || build.Published < 0 || build.Failed < 0 || build.Scanned != build.Published+build.Failed {
		return DocumentPeopleBuild{}, ErrDocumentPeopleCorrupt
	}
	if (build.State == documentPeopleBuildRunning) != (build.FinishedAt == "") || (build.State != documentPeopleBuildRunning && build.State != "completed" && build.State != documentPeopleBuildFailed) {
		return DocumentPeopleBuild{}, ErrDocumentPeopleCorrupt
	}
	return build, nil
}

// RefreshDocumentPeopleBuilds records current progress for every running
// rebuild. Failed heads stay running because the worker retries them; an
// unavailable head is terminal and makes the completed receipt failed.
func (s *Store) RefreshDocumentPeopleBuilds(ctx context.Context) error {
	var ids []string
	if err := func() error {
		rows, err := s.db.QueryContext(ctx, `SELECT operation_id FROM document_people_builds WHERE state='running' ORDER BY started_at,operation_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
			build, err := readDocumentPeopleBuild(ctx, tx, id)
			if err != nil || build.State != documentPeopleBuildRunning {
				return err
			}
			var selected, published, failed, retrying int64
			err = tx.QueryRowContext(ctx, `SELECT count(*),
				COALESCE(sum(CASE WHEN d.content_version_id IS NULL AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch>=? AND ph.resolver_fingerprint=? AND ph.state='published' THEN 1 ELSE 0 END),0),
				COALESCE(sum(CASE WHEN ph.event_generation_id=eh.generation_id AND ph.binding_epoch>=? AND ph.resolver_fingerprint=? AND ph.state IN ('failed','unavailable') THEN 1 ELSE 0 END),0),
				COALESCE(sum(CASE WHEN ph.event_generation_id=eh.generation_id AND ph.binding_epoch>=? AND ph.resolver_fingerprint=? AND ph.state='failed' THEN 1 ELSE 0 END),0)
				FROM content_versions cv JOIN document_event_heads eh ON eh.content_version_id=cv.version_id
				LEFT JOIN document_people_dirty d ON d.content_version_id=cv.version_id
				LEFT JOIN document_people_heads ph ON ph.content_version_id=cv.version_id`,
				build.TargetEpoch, build.ResolverFingerprint, build.TargetEpoch, build.ResolverFingerprint,
				build.TargetEpoch, build.ResolverFingerprint).Scan(&selected, &published, &failed, &retrying)
			if err != nil {
				return err
			}
			scanned := published + failed
			state, finishedAt := documentPeopleBuildRunning, ""
			if scanned == selected && retrying == 0 {
				state = "completed"
				if failed > 0 {
					state = documentPeopleBuildFailed
				}
				finishedAt = nowRFC3339()
			}
			if build.Scanned == scanned && build.Published == published && build.Failed == failed && state == documentPeopleBuildRunning {
				return nil
			}
			now := nowRFC3339()
			_, err = tx.ExecContext(ctx, `UPDATE document_people_builds SET scanned=?,published=?,failed=?,state=?,updated_at=?,finished_at=? WHERE operation_id=? AND state='running'`, scanned, published, failed, state, now, finishedAt, id)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DocumentPeopleCoverage(ctx context.Context) (DocumentPeopleCoverage, error) {
	var coverage DocumentPeopleCoverage
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return coverage, fmt.Errorf("starting document people coverage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRowContext(ctx, `SELECT contract_version,resolver_fingerprint,binding_epoch,publication_epoch FROM document_people_state WHERE singleton=1`).Scan(
		&coverage.ContractVersion, &coverage.ResolverFingerprint, &coverage.BindingEpoch, &coverage.PublicationEpoch)
	if err != nil {
		return coverage, err
	}
	var selected int64
	err = tx.QueryRowContext(ctx, `SELECT count(*),
		COALESCE(sum(CASE WHEN d.content_version_id IS NULL AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? AND ph.state='published' THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN d.content_version_id IS NULL AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? AND ph.state='failed' THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN d.content_version_id IS NULL AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? AND ph.state='unavailable' THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN d.content_version_id IS NULL AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? THEN ph.unresolved_actors ELSE 0 END),0),
		COALESCE(sum(CASE WHEN d.content_version_id IS NULL AND ph.event_generation_id=eh.generation_id AND ph.binding_epoch=? AND ph.resolver_fingerprint=? THEN ph.suppressed_actors ELSE 0 END),0)
		FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id LEFT JOIN document_event_heads eh ON eh.content_version_id=cv.version_id
		LEFT JOIN document_people_dirty d ON d.content_version_id=cv.version_id LEFT JOIN document_people_heads ph ON ph.content_version_id=cv.version_id WHERE n.kind='file'`,
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
