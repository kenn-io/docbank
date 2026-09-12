package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// CoverageSelection identifies a resolved policy, independently of adapter availability.
// Its zero value explicitly means that processing is unconfigured.
type CoverageSelection struct{ Configuration, ProfileFingerprint string }

// CoverageCounts partitions the distinct current collection members.
type CoverageCounts struct{ Complete, Partial, Failed, Unprocessed, None int64 }

// ProcessingCoverage describes the policy and serving snapshot behind its counts.
// Unavailable configurations carry nil Counts, never fabricated zero coverage.
type ProcessingCoverage struct {
	Configuration, ProfileFingerprint, GenerationID string
	Counts                                          *CoverageCounts
}

// ErrInvalidCoverageSelection reports an invalid or ambiguous policy selection.
var ErrInvalidCoverageSelection = errors.New("invalid coverage selection")

func normalizeCoverageSelection(selections []CoverageSelection) (CoverageSelection, error) {
	if len(selections) > 1 {
		return CoverageSelection{}, ErrInvalidCoverageSelection
	}
	selection := CoverageSelection{Configuration: "unconfigured"}
	if len(selections) == 1 {
		selection = selections[0]
	}
	if selection.Configuration == "" {
		selection.Configuration = "unconfigured"
	}
	switch selection.Configuration {
	case "configured":
		if err := validateCatalogSHA256(selection.ProfileFingerprint, "coverage profile"); err != nil {
			return CoverageSelection{}, fmt.Errorf("%w: %w", ErrInvalidCoverageSelection, err)
		}
	case "unconfigured", "profile_required":
		if selection.ProfileFingerprint != "" {
			return CoverageSelection{}, ErrInvalidCoverageSelection
		}
	default:
		return CoverageSelection{}, ErrInvalidCoverageSelection
	}
	return selection, nil
}

// collectionGenerationTx distinguishes an absent head from broken catalog
// authority. Every caller keeps this read and its aggregates in one snapshot.
func collectionGenerationTx(ctx context.Context, q metadataQuerier) (string, error) {
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rendition_lexical_heads WHERE singleton=1)`).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	generation, err := readActiveLexicalGeneration(ctx, q)
	if errors.Is(err, ErrNotFound) {
		return "", errors.New("collection serving generation has missing catalog authority")
	}
	if err != nil {
		return "", fmt.Errorf("reading collection serving generation: %w", err)
	}
	return generation.ID, nil
}

// processingCoverageCTE consumes coverage_members(node_id) and two bound
// parameters (profile, generation). It emits one processing_coverage row per
// exact live current version. Callers own both membership and the read snapshot.
func processingCoverageCTE() string {
	return `coverage_selection AS (SELECT ? AS profile, ? AS generation),
coverage_versions AS MATERIALIZED (
 SELECT n.id node_id, v.version_id, v.blob_hash
 FROM coverage_members m JOIN nodes n ON n.id=m.node_id
 JOIN content_versions v ON v.version_id=n.current_version_id AND v.node_id=n.id
 WHERE n.kind='file' AND n.trashed_at IS NULL
),
coverage_attempts AS (
 SELECT w.content_version_id, w.waiter_id, w.job_id, w.state waiter_state,
  j.state job_state, w.updated_at waiter_updated_at, j.updated_at job_updated_at,
  ROW_NUMBER() OVER (PARTITION BY w.content_version_id ORDER BY
   MAX(w.updated_at,j.updated_at) DESC, w.updated_at DESC, j.updated_at DESC,
   w.waiter_id DESC, j.job_id DESC) attempt_rank
 FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id
 JOIN coverage_versions v ON v.version_id=w.content_version_id
 CROSS JOIN coverage_selection p WHERE w.profile_fingerprint=p.profile
),
coverage_head_builds AS (
 SELECT DISTINCT b.build_id,b.completeness,b.partial_success,b.truncated,
  b.lexical_segment_count,b.unit_count,b.declared_artifact_count
 FROM coverage_versions v CROSS JOIN coverage_selection p
 JOIN rendition_heads h ON h.content_version_id=v.version_id AND h.profile_fingerprint=p.profile
 JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
  AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
 JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid AND b.source_sha256=v.blob_hash
),
coverage_verified_builds AS (
 SELECT b.build_id FROM coverage_head_builds b
 WHERE b.declared_artifact_count=(SELECT COUNT(*) FROM rendition_artifacts a WHERE a.build_id=b.build_id)
 AND b.declared_artifact_count=(SELECT COUNT(*) FROM rendition_artifacts a
  JOIN blobs blob ON blob.hash=a.blob_hash AND blob.size=a.size
  WHERE a.build_id=b.build_id AND a.checksum=a.blob_hash)
 AND b.unit_count=(SELECT COUNT(*) FROM rendition_units u WHERE u.build_id=b.build_id)
 AND b.lexical_segment_count=(SELECT COUNT(*) FROM rendition_lexical_segments s WHERE s.build_id=b.build_id)
),
coverage_serving AS (
 SELECT gb.build_id, COUNT(CASE WHEN length(trim(ix.text))>0 THEN 1 END) nonempty
 FROM rendition_lexical_generation_builds gb CROSS JOIN coverage_selection p
 LEFT JOIN rendition_lexical_index ix ON ix.build_id=gb.build_id
 WHERE gb.generation_id=p.generation GROUP BY gb.build_id
),
coverage_native AS (
 SELECT DISTINCT v.version_id FROM coverage_versions v
 JOIN text_searchable_versions sv ON sv.version_id=v.version_id
 JOIN content_fts f ON f.blob_hash=v.blob_hash CROSS JOIN coverage_selection p
 WHERE p.generation='' AND length(trim(f.text))>0
),
processing_coverage AS (
 SELECT v.node_id, v.version_id, v.blob_hash,
  COALESCE(h.attachment_id,'') attachment_id, COALESCE(h.published_at,'') published_at,
  COALESCE(b.build_id,'') build_id, COALESCE(b.completeness,'') completeness,
  COALESCE(b.partial_success,0) partial_success, COALESCE(b.truncated,0) truncated,
  COALESCE(b.lexical_segment_count,0) lexical_segment_count,
  verified.build_id IS NOT NULL verified, serving.build_id IS NOT NULL serving,
  COALESCE(attempt.waiter_id,'') waiter_id, COALESCE(attempt.job_id,'') job_id,
  COALESCE(attempt.waiter_state,'') waiter_state, COALESCE(attempt.job_state,'') job_state,
  COALESCE(attempt.waiter_updated_at,'') waiter_updated_at, COALESCE(attempt.job_updated_at,'') job_updated_at,
  CASE
   WHEN verified.build_id IS NOT NULL AND serving.nonempty>0 THEN
    CASE WHEN b.truncated OR b.partial_success OR b.completeness='partial' THEN 'partial' ELSE 'complete' END
   WHEN verified.build_id IS NOT NULL AND serving.build_id IS NOT NULL AND b.lexical_segment_count=0 THEN 'none'
   WHEN native.version_id IS NOT NULL THEN 'complete'
   WHEN attempt.waiter_state='rejected' OR attempt.job_state IN ('failed','operator_required') THEN 'failed'
   ELSE 'unprocessed'
  END state
 FROM coverage_versions v CROSS JOIN coverage_selection p
 LEFT JOIN rendition_heads h ON h.content_version_id=v.version_id AND h.profile_fingerprint=p.profile
 LEFT JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
  AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
 LEFT JOIN coverage_head_builds b ON b.build_id=a.build_id
 LEFT JOIN coverage_verified_builds verified ON verified.build_id=b.build_id
 LEFT JOIN coverage_serving serving ON serving.build_id=b.build_id
 LEFT JOIN coverage_native native ON native.version_id=v.version_id
 LEFT JOIN coverage_attempts attempt ON attempt.content_version_id=v.version_id AND attempt.attempt_rank=1
)`
}

func collectionCoverageTx(ctx context.Context, q metadataQuerier, ids []string, selection CoverageSelection, generation string) (map[string]ProcessingCoverage, error) {
	result := make(map[string]ProcessingCoverage, len(ids))
	for _, id := range ids {
		coverage := ProcessingCoverage{Configuration: selection.Configuration, ProfileFingerprint: selection.ProfileFingerprint, GenerationID: generation}
		if selection.Configuration == "configured" {
			coverage.Counts = &CoverageCounts{}
		}
		result[id] = coverage
	}
	if len(ids) == 0 || selection.Configuration != "configured" {
		return result, nil
	}
	// A configured policy can precede its first catalog use. Existing catalog
	// records must still pass canonical validation rather than hiding corruption.
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM processing_profiles WHERE profile_fingerprint=?)`, selection.ProfileFingerprint).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		if _, err := loadProcessingProfile(ctx, q, selection.ProfileFingerprint); err != nil {
			return nil, err
		}
	}
	args := make([]any, 0, len(ids)+2)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, selection.ProfileFingerprint, generation)
	rows, err := q.QueryContext(ctx, `WITH `+CollectionMembershipCTE+`,
 selected_members AS (SELECT * FROM collection_members WHERE ingest_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)),
 coverage_members AS (SELECT DISTINCT node_id FROM selected_members), `+processingCoverageCTE()+`
 SELECT m.ingest_id, p.state, COUNT(*) FROM selected_members m
 JOIN processing_coverage p ON p.node_id=m.node_id GROUP BY m.ingest_id,p.state`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading collection coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, state string
		var count int64
		if err := rows.Scan(&id, &state, &count); err != nil {
			return nil, err
		}
		addCoverageCount(result[id].Counts, state, count)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, rows.Close()
}

func addCoverageCount(counts *CoverageCounts, state string, count int64) {
	switch state {
	case "complete":
		counts.Complete += count
	case "partial":
		counts.Partial += count
	case "failed":
		counts.Failed += count
	case "none":
		counts.None += count
	default:
		counts.Unprocessed += count
	}
}
