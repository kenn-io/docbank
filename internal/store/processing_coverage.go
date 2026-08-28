package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

// ProcessingCoverageScope names one bounded exact-version coverage snapshot.
type ProcessingCoverageScope struct {
	ContentVersionIDs            []string
	ProcessingProfileFingerprint string
	Bindings                     []ProcessingCoverageBinding
}

type ProcessingCoverageBinding struct {
	BindingID string
	Required  bool
}

// ProcessingClassCoverage counts mutually exclusive states. A rebuilding cell
// also records whether its previous complete generation is still available.
type ProcessingClassCoverage struct {
	Name                      string
	Required                  bool
	State                     string
	Complete                  int
	Unavailable               int
	Stale                     int
	Ineligible                int
	Rebuilding                int
	PreviousGenerationServing int
	Total                     int
}

type ProcessingCoverageSnapshot struct {
	Renditions ProcessingClassCoverage
	Embeddings []ProcessingClassCoverage
}

// ProcessingCoverage evaluates source visibility, retained heads and replacement
// jobs in one snapshot, using the same embedding authority as document search.
func (s *Store) ProcessingCoverage(ctx context.Context, scope ProcessingCoverageScope) (ProcessingCoverageSnapshot, error) {
	if len(scope.ContentVersionIDs) == 0 || len(scope.Bindings) > 64 {
		return ProcessingCoverageSnapshot{}, errors.New("processing coverage scope is outside bounded counts")
	}
	if err := validateCatalogSHA256(scope.ProcessingProfileFingerprint, "coverage profile fingerprint"); err != nil {
		return ProcessingCoverageSnapshot{}, err
	}
	opts, err := s.NormalizeSearchOptions(ctx, SearchOptions{ContentVersionIDs: scope.ContentVersionIDs})
	if err != nil {
		return ProcessingCoverageSnapshot{}, err
	}
	seen := make(map[string]struct{}, len(scope.Bindings))
	for _, binding := range scope.Bindings {
		if err := validateEmbeddingCatalogText(binding.BindingID, "coverage binding ID"); err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		if _, duplicate := seen[binding.BindingID]; duplicate {
			return ProcessingCoverageSnapshot{}, errors.New("coverage scope contains duplicate bindings")
		}
		seen[binding.BindingID] = struct{}{}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProcessingCoverageSnapshot{}, fmt.Errorf("starting processing coverage snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, profileErr := loadProcessingProfile(ctx, tx, scope.ProcessingProfileFingerprint)
	if profileErr != nil && !errors.Is(profileErr, ErrNotFound) {
		return ProcessingCoverageSnapshot{}, profileErr
	}
	result := ProcessingCoverageSnapshot{Renditions: ProcessingClassCoverage{Name: "rendition", Total: len(opts.ContentVersionIDs)}}
	live := make(map[string]bool, len(opts.ContentVersionIDs))
	for _, versionID := range opts.ContentVersionIDs {
		var current bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions v
			JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
			WHERE v.version_id=? AND n.kind='file')`, versionID).Scan(&current); err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		live[versionID] = current
		if !current {
			result.Renditions.Stale++
			continue
		}
		complete, err := processingRenditionAvailableTx(ctx, tx, versionID, scope.ProcessingProfileFingerprint)
		if err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		var rebuilding bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rendition_job_waiters w
			JOIN rendition_jobs j ON j.job_id=w.job_id
			WHERE w.content_version_id=? AND w.profile_fingerprint=? AND w.state='waiting'
			AND j.state IN ('queued','running','retry_wait'))`, versionID, scope.ProcessingProfileFingerprint).Scan(&rebuilding); err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		countProcessingCoverage(&result.Renditions, complete, rebuilding)
	}
	result.Renditions.State = processingCoverageClassState(result.Renditions)
	for _, requested := range scope.Bindings {
		item := ProcessingClassCoverage{Name: requested.BindingID, Required: requested.Required, Total: len(opts.ContentVersionIDs)}
		var binding document.EmbeddingBindingV1
		var vectorSpace string
		if profileErr == nil {
			resolved, fingerprints, err := embeddingProfileBindingAuthority(ctx, tx,
				scope.ProcessingProfileFingerprint, requested.BindingID)
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			binding = resolved
			if (binding.Activation == document.EmbeddingRequired) != requested.Required {
				return ProcessingCoverageSnapshot{}, errors.New("coverage binding does not match processing profile authority")
			}
			vectorSpace = fingerprints.VectorSpace[binding.Name]
		}
		for _, versionID := range opts.ContentVersionIDs {
			if !live[versionID] {
				item.Stale++
				continue
			}
			if profileErr != nil {
				item.Unavailable++
				continue
			}
			_, complete, err := semanticSearchCoverageTx(ctx, tx, scope.ProcessingProfileFingerprint,
				binding.Name, binding.InputKind, vectorSpace, SearchOptions{ContentVersionIDs: []string{versionID}})
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			var rebuilding bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_jobs j
				JOIN embedding_input_generations g ON g.generation_id=j.generation_id
				WHERE j.content_version_id=? AND j.profile_fingerprint=? AND j.binding_id=?
				AND j.input_kind=? AND j.vector_space_id=? AND j.state IN ('queued','running','retry_wait')
				AND (j.input_kind='original_file' OR EXISTS (
					SELECT 1 FROM rendition_heads h WHERE h.content_version_id=j.content_version_id
					AND h.profile_fingerprint=j.profile_fingerprint AND h.attachment_id=g.attachment_id)))`,
				versionID, scope.ProcessingProfileFingerprint, binding.Name, binding.InputKind, vectorSpace).Scan(&rebuilding); err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			countProcessingCoverage(&item, complete == 1, rebuilding)
		}
		item.State = processingCoverageClassState(item)
		result.Embeddings = append(result.Embeddings, item)
	}
	if err := tx.Commit(); err != nil {
		return ProcessingCoverageSnapshot{}, fmt.Errorf("closing processing coverage snapshot: %w", err)
	}
	return result, nil
}

func processingRenditionAvailableTx(ctx context.Context, tx *sql.Tx, versionID, profile string) (bool, error) {
	var attachmentID string
	err := tx.QueryRowContext(ctx, `SELECT attachment_id FROM rendition_heads
		WHERE content_version_id=? AND profile_fingerprint=?`, versionID, profile).Scan(&attachmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	attachment, err := loadRenditionAttachment(ctx, tx, attachmentID)
	if err != nil {
		return false, err
	}
	build, err := loadRenditionBuild(ctx, tx, attachment.BuildID)
	if err != nil {
		return false, err
	}
	if err := validateRenditionArtifactRolesForProfile(attachment.Profile, build); err != nil {
		return false, err
	}
	return true, nil
}

func countProcessingCoverage(item *ProcessingClassCoverage, complete, rebuilding bool) {
	switch {
	case rebuilding:
		item.Rebuilding++
		if complete {
			item.PreviousGenerationServing++
		}
	case complete:
		item.Complete++
	default:
		item.Unavailable++
	}
}

func processingCoverageClassState(item ProcessingClassCoverage) string {
	switch {
	case item.Stale == item.Total:
		return "stale"
	case item.Rebuilding > 0:
		return "rebuilding"
	case item.Complete == item.Total:
		return "complete"
	case item.Complete > 0:
		return "partial"
	default:
		return "unavailable"
	}
}
