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
	filterSQL, filterArgs := searchFilterSQL(opts)
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(h.attachment_id,''),
		EXISTS(SELECT 1 FROM rendition_job_waiters w
			JOIN rendition_jobs j ON j.job_id=w.job_id
			WHERE w.content_version_id=cv.version_id AND w.profile_fingerprint=? AND w.state='waiting'
			AND j.state IN ('queued','running','retry_wait'))
		FROM `+nodeFrom+`
		LEFT JOIN rendition_heads h ON h.content_version_id=cv.version_id AND h.profile_fingerprint=?
		WHERE n.kind='file' AND n.trashed_at IS NULL AND cv.version_id IS NOT NULL `+filterSQL,
		append([]any{scope.ProcessingProfileFingerprint, scope.ProcessingProfileFingerprint}, filterArgs...)...)
	if err != nil {
		return ProcessingCoverageSnapshot{}, err
	}
	defer func() { _ = rows.Close() }()
	type renditionHead struct {
		attachmentID string
		rebuilding   bool
	}
	var heads []renditionHead
	for rows.Next() {
		var head renditionHead
		if err := rows.Scan(&head.attachmentID, &head.rebuilding); err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		heads = append(heads, head)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return ProcessingCoverageSnapshot{}, err
	}
	result.Renditions.Stale = len(opts.ContentVersionIDs) - len(heads)
	for _, head := range heads {
		complete, err := processingRenditionAvailableTx(ctx, tx, head.attachmentID)
		if err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		countProcessingCoverage(&result.Renditions, complete, head.rebuilding)
	}
	result.Renditions.State = processingCoverageClassState(result.Renditions)
	for _, requested := range scope.Bindings {
		item := ProcessingClassCoverage{Name: requested.BindingID, Required: requested.Required,
			Total: len(opts.ContentVersionIDs), Stale: result.Renditions.Stale, Unavailable: len(heads)}
		if profileErr == nil {
			binding, fingerprints, err := embeddingProfileBindingAuthority(ctx, tx,
				scope.ProcessingProfileFingerprint, requested.BindingID)
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			if (binding.Activation == document.EmbeddingRequired) != requested.Required {
				return ProcessingCoverageSnapshot{}, errors.New("coverage binding does not match processing profile authority")
			}
			vectorSpace := fingerprints.VectorSpace[binding.Name]
			_, complete, err := semanticSearchCoverageTx(ctx, tx, scope.ProcessingProfileFingerprint,
				binding.Name, binding.InputKind, vectorSpace, opts)
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			rows, err := tx.QueryContext(ctx, `SELECT DISTINCT cv.version_id FROM `+nodeFrom+`
				JOIN embedding_jobs j ON j.content_version_id=cv.version_id
				JOIN embedding_input_generations g ON g.generation_id=j.generation_id
				WHERE n.kind='file' AND n.trashed_at IS NULL AND j.profile_fingerprint=? AND j.binding_id=?
				AND j.input_kind=? AND j.vector_space_id=? AND j.state IN ('queued','running','retry_wait')
				AND (j.input_kind='original_file' OR EXISTS (
					SELECT 1 FROM rendition_heads h WHERE h.content_version_id=j.content_version_id
					AND h.profile_fingerprint=j.profile_fingerprint AND h.attachment_id=g.attachment_id)) `+filterSQL,
				append([]any{scope.ProcessingProfileFingerprint, binding.Name, binding.InputKind, vectorSpace}, filterArgs...)...)
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			defer func() { _ = rows.Close() }()
			var rebuildingIDs []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return ProcessingCoverageSnapshot{}, err
				}
				rebuildingIDs = append(rebuildingIDs, id)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			item.Rebuilding = len(rebuildingIDs)
			if item.Rebuilding > 0 {
				_, item.PreviousGenerationServing, err = semanticSearchCoverageTx(ctx, tx, scope.ProcessingProfileFingerprint,
					binding.Name, binding.InputKind, vectorSpace, SearchOptions{ContentVersionIDs: rebuildingIDs})
				if err != nil {
					return ProcessingCoverageSnapshot{}, err
				}
			}
			item.Complete = complete - item.PreviousGenerationServing
			item.Unavailable -= item.Complete + item.Rebuilding
		}
		item.State = processingCoverageClassState(item)
		result.Embeddings = append(result.Embeddings, item)
	}
	if err := tx.Commit(); err != nil {
		return ProcessingCoverageSnapshot{}, fmt.Errorf("closing processing coverage snapshot: %w", err)
	}
	return result, nil
}

func processingRenditionAvailableTx(ctx context.Context, tx *sql.Tx, attachmentID string) (bool, error) {
	if attachmentID == "" {
		return false, nil
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
