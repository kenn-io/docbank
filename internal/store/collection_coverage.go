package store

import (
	"context"
	"errors"
	"fmt"
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

func normalizeCoverageSelection(selection CoverageSelection) (CoverageSelection, error) {
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
// Build integrity is validated when staging, publishing, and restoring the catalog.
func processingCoverageCTE() string {
	return `coverage_selection AS (SELECT ? AS profile, ? AS generation),
coverage_versions AS MATERIALIZED (
 SELECT n.id node_id, v.version_id, v.blob_hash
 FROM coverage_members m JOIN nodes n ON n.id=m.node_id
 JOIN content_versions v ON v.version_id=n.current_version_id AND v.node_id=n.id
 WHERE n.kind='file' AND n.trashed_at IS NULL
), ` + processingVersionCoverageCTE()
}

// processingVersionCoverageCTE classifies exact versions supplied by the caller
// in coverage_versions, under the profile/generation in coverage_selection.
func processingVersionCoverageCTE() string {
	return `coverage_attempts AS (
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
  b.lexical_segment_count
 FROM coverage_versions v CROSS JOIN coverage_selection p
 JOIN rendition_heads h ON h.content_version_id=v.version_id AND h.profile_fingerprint=p.profile
 JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
  AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
 JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid AND b.source_sha256=v.blob_hash
),
coverage_serving AS MATERIALIZED (
 SELECT b.build_id, EXISTS(
  SELECT 1 FROM rendition_lexical_index ix WHERE ix.build_id=b.build_id AND length(trim(ix.text))>0
 ) nonempty
 FROM coverage_head_builds b CROSS JOIN coverage_selection p
 WHERE EXISTS(SELECT 1 FROM rendition_lexical_generation_builds gb
  WHERE gb.generation_id=p.generation AND gb.build_id=b.build_id)
),
coverage_native AS (
 SELECT v.version_id FROM coverage_versions v CROSS JOIN coverage_selection p
 WHERE p.profile<>'' AND p.generation=''
 AND EXISTS(SELECT 1 FROM text_searchable_versions sv WHERE sv.version_id=v.version_id)
 AND EXISTS(SELECT 1 FROM extracted_text e WHERE e.blob_hash=v.blob_hash AND e.status='ok' AND length(trim(e.text))>0)
),
processing_coverage AS (
 SELECT v.node_id, v.version_id, v.blob_hash,
  COALESCE(h.attachment_id,'') attachment_id, COALESCE(h.published_at,'') published_at,
  COALESCE(b.build_id,'') build_id, COALESCE(b.completeness,'') completeness,
  COALESCE(b.partial_success,0) partial_success, COALESCE(b.truncated,0) truncated,
  COALESCE(b.lexical_segment_count,0) lexical_segment_count,
  serving.build_id IS NOT NULL serving,
  COALESCE(attempt.waiter_id,'') waiter_id, COALESCE(attempt.job_id,'') job_id,
  COALESCE(attempt.waiter_state,'') waiter_state, COALESCE(attempt.job_state,'') job_state,
  COALESCE(attempt.waiter_updated_at,'') waiter_updated_at, COALESCE(attempt.job_updated_at,'') job_updated_at,
  CASE
   WHEN serving.nonempty>0 THEN
    CASE WHEN b.truncated OR b.partial_success OR b.completeness='partial' THEN 'partial' ELSE 'complete' END
   WHEN serving.nonempty=0 THEN 'none'
   WHEN native.version_id IS NOT NULL THEN 'complete'
   WHEN attempt.waiter_state='rejected' OR attempt.job_state IN ('failed','operator_required') THEN 'failed'
   ELSE 'unprocessed'
  END state
 FROM coverage_versions v CROSS JOIN coverage_selection p
 LEFT JOIN rendition_heads h ON h.content_version_id=v.version_id AND h.profile_fingerprint=p.profile
 LEFT JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
  AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
 LEFT JOIN coverage_head_builds b ON b.build_id=a.build_id
 LEFT JOIN coverage_serving serving ON serving.build_id=b.build_id
 LEFT JOIN coverage_native native ON native.version_id=v.version_id
 LEFT JOIN coverage_attempts attempt ON attempt.content_version_id=v.version_id AND attempt.attempt_rank=1
)`
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
