package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const maxBatesCandidateEvidence = 25

var ErrInvalidBatesSelector = errors.New("exactly one Bates export selector is required")

type BatesArtifactSelector struct {
	BatesLabel     string
	CustodianLabel string
	PersonID       string
}

type BatesArtifactPosition struct {
	CreatedAt  string
	ArtifactID string
}

type BatesArtifactEvidence struct {
	Kind          string
	OccurrenceID  string
	Label         string
	OutputPage    int
	AssignmentID  string
	ScopeKind     string
	RawLabel      string
	PersonID      string
	Rank          string
	Basis         string
	PackageID     string
	PackageRecord string
}

type BatesArtifactCandidate struct {
	ArtifactID, AllocationID, SnapshotID, BlobSHA256, MediaType, ManifestSHA256, State, CreatedAt string
	Size                                                                                          int64
	PageCount                                                                                     int
	Evidence                                                                                      []BatesArtifactEvidence
	EvidenceTruncated                                                                             bool
	EvidenceNext                                                                                  int
}

type BatesArtifactCandidatePage struct {
	Items        []BatesArtifactCandidate
	Next         BatesArtifactPosition
	BindingEpoch int64
}

func (s BatesArtifactSelector) normalized(ctx context.Context, tx *sql.Tx) (BatesArtifactSelector, error) {
	selected := 0
	if s.BatesLabel != "" {
		selected++
	}
	if s.CustodianLabel != "" {
		selected++
	}
	if s.PersonID != "" {
		selected++
	}
	if selected != 1 || !utf8.ValidString(s.BatesLabel) || !utf8.ValidString(s.CustodianLabel) {
		return BatesArtifactSelector{}, ErrInvalidBatesSelector
	}
	if s.CustodianLabel != "" {
		s.CustodianLabel = document.FoldPersonName(s.CustodianLabel)
		if s.CustodianLabel == "" || len(s.CustodianLabel) > document.MaxPersonDisplayNameBytes {
			return BatesArtifactSelector{}, ErrInvalidBatesSelector
		}
	}
	if s.PersonID != "" {
		personID, err := resolvePersonIDTx(ctx, tx, s.PersonID)
		if err != nil {
			return BatesArtifactSelector{}, ErrInvalidBatesSelector
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM persons WHERE person_id=?`, personID).Scan(&state); err != nil || state == "retired" {
			return BatesArtifactSelector{}, ErrInvalidBatesSelector
		}
		s.PersonID = personID
	}
	return s, nil
}

func (s *Store) FindBatesArtifacts(
	ctx context.Context, selector BatesArtifactSelector, after BatesArtifactPosition, limit int,
) (BatesArtifactCandidatePage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BatesArtifactCandidatePage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	selector, err = selector.normalized(ctx, tx)
	if err != nil || limit < 1 || limit > 250 || (after.CreatedAt == "") != (after.ArtifactID == "") ||
		after.ArtifactID != "" && validateUUIDv4(after.ArtifactID) != nil {
		return BatesArtifactCandidatePage{}, ErrInvalidBatesSelector
	}
	var epoch int64
	if selector.CustodianLabel != "" || selector.PersonID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch); err != nil {
			return BatesArtifactCandidatePage{}, err
		}
	}
	predicate, args := batesCandidatePredicate(selector)
	query := `SELECT DISTINCT a.artifact_id,a.allocation_id,l.snapshot_id,a.blob_hash,a.size,a.media_type,a.page_count,
		a.manifest_sha256,a.state,a.created_at FROM bates_artifacts a
		JOIN bates_allocations l USING(allocation_id) JOIN bates_artifact_pages ap USING(artifact_id)
		JOIN collection_snapshot_members sm ON sm.snapshot_id=l.snapshot_id AND sm.occurrence_id=ap.occurrence_id
		JOIN collection_snapshots cs ON cs.snapshot_id=l.snapshot_id
		WHERE (` + predicate + `) AND (?='' OR a.created_at>? OR (a.created_at=? AND a.artifact_id>?))
		ORDER BY a.created_at,a.artifact_id LIMIT ?`
	args = append(args, after.CreatedAt, after.CreatedAt, after.CreatedAt, after.ArtifactID, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return BatesArtifactCandidatePage{}, err
	}
	defer func() { _ = rows.Close() }()
	page := BatesArtifactCandidatePage{BindingEpoch: epoch}
	for rows.Next() {
		var candidate BatesArtifactCandidate
		if err := rows.Scan(&candidate.ArtifactID, &candidate.AllocationID, &candidate.SnapshotID, &candidate.BlobSHA256,
			&candidate.Size, &candidate.MediaType, &candidate.PageCount, &candidate.ManifestSHA256, &candidate.State,
			&candidate.CreatedAt); err != nil {
			return BatesArtifactCandidatePage{}, err
		}
		page.Items = append(page.Items, candidate)
	}
	if err := rows.Err(); err != nil {
		return BatesArtifactCandidatePage{}, err
	}
	more := len(page.Items) > limit
	if more {
		page.Items = page.Items[:limit]
	}
	for index := range page.Items {
		evidence, truncated, err := batesCandidateEvidence(ctx, tx, selector, page.Items[index].ArtifactID, 0)
		if err != nil {
			return BatesArtifactCandidatePage{}, err
		}
		page.Items[index].Evidence, page.Items[index].EvidenceTruncated = evidence, truncated
		if truncated {
			page.Items[index].EvidenceNext = len(evidence)
		}
	}
	if more {
		last := page.Items[len(page.Items)-1]
		page.Next = BatesArtifactPosition{CreatedAt: last.CreatedAt, ArtifactID: last.ArtifactID}
	}
	return page, tx.Commit()
}

// FindBatesArtifactEvidence continues the bounded evidence for one candidate
// under the same normalized selector and person-binding epoch.
func (s *Store) FindBatesArtifactEvidence(
	ctx context.Context, selector BatesArtifactSelector, artifactID string, offset int,
) (BatesArtifactCandidatePage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BatesArtifactCandidatePage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	selector, err = selector.normalized(ctx, tx)
	if err != nil || validateUUIDv4(artifactID) != nil || offset < 1 || offset > MaxSnapshotPages {
		return BatesArtifactCandidatePage{}, ErrInvalidBatesSelector
	}
	page := BatesArtifactCandidatePage{}
	if selector.CustodianLabel != "" || selector.PersonID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&page.BindingEpoch); err != nil {
			return BatesArtifactCandidatePage{}, err
		}
	}
	predicate, args := batesCandidatePredicate(selector)
	args = append([]any{artifactID}, args...)
	var candidate BatesArtifactCandidate
	err = tx.QueryRowContext(ctx, `SELECT DISTINCT a.artifact_id,a.allocation_id,l.snapshot_id,a.blob_hash,a.size,
		a.media_type,a.page_count,a.manifest_sha256,a.state,a.created_at FROM bates_artifacts a
		JOIN bates_allocations l USING(allocation_id) JOIN bates_artifact_pages ap USING(artifact_id)
		JOIN collection_snapshot_members sm ON sm.snapshot_id=l.snapshot_id AND sm.occurrence_id=ap.occurrence_id
		JOIN collection_snapshots cs ON cs.snapshot_id=l.snapshot_id
		WHERE a.artifact_id=? AND (`+predicate+`)`, args...).Scan(&candidate.ArtifactID, &candidate.AllocationID,
		&candidate.SnapshotID, &candidate.BlobSHA256, &candidate.Size, &candidate.MediaType, &candidate.PageCount,
		&candidate.ManifestSHA256, &candidate.State, &candidate.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BatesArtifactCandidatePage{}, ErrNotFound
	}
	if err != nil {
		return BatesArtifactCandidatePage{}, err
	}
	candidate.Evidence, candidate.EvidenceTruncated, err = batesCandidateEvidence(ctx, tx, selector, artifactID, offset)
	if err != nil {
		return BatesArtifactCandidatePage{}, err
	}
	if candidate.EvidenceTruncated {
		candidate.EvidenceNext = offset + len(candidate.Evidence)
	}
	page.Items = []BatesArtifactCandidate{candidate}
	return page, tx.Commit()
}

func batesCandidatePredicate(selector BatesArtifactSelector) (string, []any) {
	if selector.BatesLabel != "" {
		return "ap.label=?", []any{selector.BatesLabel}
	}
	claim := `(ca.scope_kind='document' AND ca.node_id=sm.node_id AND ca.content_version_id=sm.content_version_id) OR
		(ca.scope_kind='package' AND ca.package_id=p.package_id AND
		 (COALESCE(ca.package_record_id,'')='' OR ca.package_record_id=pr.row_id)) OR
		(ca.scope_kind='collection' AND EXISTS(SELECT 1 FROM json_each(sm.canonical_json,'$.source_collection_ids') source
		 WHERE source.value=ca.ingest_id))`
	match := "ca.raw_label_folded=?"
	value := any(selector.CustodianLabel)
	if selector.PersonID != "" {
		match, value = "ca.person_id=?", selector.PersonID
	}
	return `EXISTS(SELECT 1 FROM custodian_assignments ca
		LEFT JOIN packages p ON p.snapshot_id=l.snapshot_id
		LEFT JOIN package_records pr ON pr.package_id=p.package_id AND pr.occurrence_id=ap.occurrence_id
		WHERE ca.retired_at IS NULL AND (` + claim + `) AND ` + match + `)`, []any{value}
}

func batesCandidateEvidence(ctx context.Context, q metadataQuerier, selector BatesArtifactSelector, artifactID string, offset int) ([]BatesArtifactEvidence, bool, error) {
	if selector.BatesLabel != "" {
		rows, err := q.QueryContext(ctx, `SELECT occurrence_id,label,output_page FROM bates_artifact_pages
			WHERE artifact_id=? AND label=? ORDER BY ordinal LIMIT ? OFFSET ?`, artifactID, selector.BatesLabel,
			maxBatesCandidateEvidence+1, offset)
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = rows.Close() }()
		var values []BatesArtifactEvidence
		for rows.Next() {
			var value BatesArtifactEvidence
			value.Kind = "label"
			if err := rows.Scan(&value.OccurrenceID, &value.Label, &value.OutputPage); err != nil {
				return nil, false, err
			}
			values = append(values, value)
		}
		return boundedBatesEvidence(values), len(values) > maxBatesCandidateEvidence, rows.Err()
	}
	match := "ca.raw_label_folded=?"
	value := any(selector.CustodianLabel)
	if selector.PersonID != "" {
		match, value = "ca.person_id=?", selector.PersonID
	}
	query := `SELECT DISTINCT ap.occurrence_id,ca.assignment_id,ca.scope_kind,ca.raw_label,ca.person_id,ca.rank,ca.basis,
		COALESCE(ca.package_id,''),COALESCE(ca.package_record_id,'')
		FROM bates_artifact_pages ap JOIN bates_artifacts a USING(artifact_id) JOIN bates_allocations l USING(allocation_id)
		JOIN collection_snapshot_members sm ON sm.snapshot_id=l.snapshot_id AND sm.occurrence_id=ap.occurrence_id
		JOIN collection_snapshots cs ON cs.snapshot_id=l.snapshot_id
		LEFT JOIN packages p ON p.snapshot_id=l.snapshot_id
		LEFT JOIN package_records pr ON pr.package_id=p.package_id AND pr.occurrence_id=ap.occurrence_id
		JOIN custodian_assignments ca ON ca.retired_at IS NULL AND (
		 (ca.scope_kind='document' AND ca.node_id=sm.node_id AND ca.content_version_id=sm.content_version_id) OR
		 (ca.scope_kind='package' AND ca.package_id=p.package_id AND (COALESCE(ca.package_record_id,'')='' OR ca.package_record_id=pr.row_id)) OR
		 (ca.scope_kind='collection' AND EXISTS(SELECT 1 FROM json_each(sm.canonical_json,'$.source_collection_ids') source
		  WHERE source.value=ca.ingest_id)))
		WHERE ap.artifact_id=? AND ` + match + ` ORDER BY ap.occurrence_id,ca.assignment_id LIMIT ? OFFSET ?`
	rows, err := q.QueryContext(ctx, query, artifactID, value, maxBatesCandidateEvidence+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var values []BatesArtifactEvidence
	for rows.Next() {
		var item BatesArtifactEvidence
		var person sql.NullString
		item.Kind = "custodian"
		if err := rows.Scan(&item.OccurrenceID, &item.AssignmentID, &item.ScopeKind, &item.RawLabel, &person,
			&item.Rank, &item.Basis, &item.PackageID, &item.PackageRecord); err != nil {
			return nil, false, err
		}
		if person.Valid {
			item.PersonID = person.String
		}
		values = append(values, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("reading Bates candidate evidence: %w", err)
	}
	return boundedBatesEvidence(values), len(values) > maxBatesCandidateEvidence, nil
}

func boundedBatesEvidence(values []BatesArtifactEvidence) []BatesArtifactEvidence {
	if len(values) > maxBatesCandidateEvidence {
		return values[:maxBatesCandidateEvidence]
	}
	return values
}
