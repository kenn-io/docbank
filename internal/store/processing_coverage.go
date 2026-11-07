package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

// ProcessingCoverageScope selects either bounded exact versions or current published notes.
type ProcessingCoverageScope struct {
	ContentVersionIDs            []string
	PublishedNotes               *PublishedNoteSelector
	ProcessingProfileFingerprint string
	Bindings                     []ProcessingCoverageBinding
}

type ProcessingCoverageBinding struct {
	VectorSpaceID string
	BindingID     string
	Required      bool
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
	if (len(scope.ContentVersionIDs) == 0 && scope.PublishedNotes == nil) ||
		(len(scope.ContentVersionIDs) != 0 && scope.PublishedNotes != nil) || len(scope.Bindings) > 64 {
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
	if scope.PublishedNotes != nil {
		return s.publishedNoteProcessingCoverage(ctx, scope)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return ProcessingCoverageSnapshot{}, fmt.Errorf("starting processing coverage snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, profileErr := loadProcessingProfile(ctx, tx, scope.ProcessingProfileFingerprint)
	if profileErr != nil && !errors.Is(profileErr, ErrNotFound) {
		return ProcessingCoverageSnapshot{}, profileErr
	}
	result := ProcessingCoverageSnapshot{Renditions: ProcessingClassCoverage{Name: "rendition", Total: len(opts.ContentVersionIDs)}}
	d := metadataJSONDialect{s.postgresSchema != ""}
	versionsJSON, err := json.Marshal(opts.ContentVersionIDs)
	if err != nil {
		return ProcessingCoverageSnapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(h.attachment_id,''),
		COALESCE(a.build_id,''), COALESCE(a.profile_fingerprint,''),
		EXISTS(SELECT 1 FROM rendition_job_waiters w
			JOIN rendition_jobs j ON j.job_id=w.job_id
			WHERE w.content_version_id=cv.version_id AND w.profile_fingerprint=$1 AND w.state='waiting'
			AND `+d.originalFileWaiter()+` AND j.state IN ('queued','running','retry_wait'))
		FROM `+nodeFrom+`
		LEFT JOIN rendition_heads h ON h.content_version_id=cv.version_id AND h.profile_fingerprint=$1
		LEFT JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		WHERE n.kind='file' AND n.trashed_at IS NULL AND cv.version_id IS NOT NULL AND cv.version_id IN (SELECT value FROM `+d.arrayValues("$2")+`)`,
		scope.ProcessingProfileFingerprint, string(versionsJSON))
	if err != nil {
		return ProcessingCoverageSnapshot{}, err
	}
	defer func() { _ = rows.Close() }()
	type renditionValidationKey struct {
		buildID, profileFingerprint string
	}
	type renditionHead struct {
		attachmentID string
		rebuilding   bool
		key          renditionValidationKey
	}
	heads := make([]renditionHead, 0, len(opts.ContentVersionIDs))
	for rows.Next() {
		var head renditionHead
		if err := rows.Scan(&head.attachmentID, &head.key.buildID, &head.key.profileFingerprint, &head.rebuilding); err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		heads = append(heads, head)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return ProcessingCoverageSnapshot{}, err
	}
	result.Renditions.Stale = len(opts.ContentVersionIDs) - len(heads)
	// Shared builds need full validation only once per attachment profile in
	// this read snapshot. Keep counting each version's state independently.
	validated := make(map[renditionValidationKey]bool)
	for _, head := range heads {
		complete := validated[head.key]
		if !complete {
			complete, err = processingRenditionAvailableTx(ctx, tx, head.attachmentID)
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			if complete {
				validated[head.key] = true
			}
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
			if requested.VectorSpaceID != "" {
				vectorSpace = requested.VectorSpaceID
			}
			complete, err := s.processingEmbeddingCompleteTx(ctx, tx, scope.ProcessingProfileFingerprint,
				binding.Name, binding.InputKind, vectorSpace, opts.ContentVersionIDs)
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			rows, err := tx.QueryContext(ctx, `SELECT DISTINCT cv.version_id FROM `+nodeFrom+`
				JOIN embedding_jobs j ON j.content_version_id=cv.version_id
				JOIN embedding_input_generations g ON g.generation_id=j.generation_id
				WHERE n.kind='file' AND n.trashed_at IS NULL AND j.profile_fingerprint=$1 AND j.binding_id=$2
				AND j.input_kind=$3 AND j.vector_space_id=$4 AND j.state IN ('queued','running','retry_wait')
				AND (j.input_kind='original_file' OR (j.input_kind='image' AND `+currentImageEmbeddingSQL("j.content_version_id", "g.evidence_fingerprint")+`) OR (j.input_kind='rendition_chunk' AND EXISTS (
					SELECT 1 FROM rendition_heads h WHERE h.content_version_id=j.content_version_id
					AND h.profile_fingerprint=j.profile_fingerprint AND h.attachment_id=g.attachment_id))) AND cv.version_id IN (SELECT value FROM `+d.arrayValues("$5")+`)`,
				scope.ProcessingProfileFingerprint, binding.Name, binding.InputKind, vectorSpace, string(versionsJSON))
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
				item.PreviousGenerationServing, err = s.processingEmbeddingCompleteTx(ctx, tx, scope.ProcessingProfileFingerprint,
					binding.Name, binding.InputKind, vectorSpace, rebuildingIDs)
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

// processingEmbeddingCompleteTx counts the exact version fence without applying
// the general search filters, which this API does not accept.
func (s *Store) processingEmbeddingCompleteTx(ctx context.Context, tx *sql.Tx, profile, binding string,
	kind document.EmbeddingInputKind, space string, versions []string,
) (int, error) {
	encoded, err := json.Marshal(versions)
	if err != nil {
		return 0, err
	}
	var count int
	d := metadataJSONDialect{s.postgresSchema != ""}
	err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT n.id) `+currentEmbeddingHeadsSQL()+`
 AND eh.profile_fingerprint=$1 AND eh.binding_id=$2 AND eh.input_kind=$3 AND eh.vector_space_id=$4
 AND cv.version_id IN (SELECT value FROM `+d.arrayValues("$5")+`)`, profile, binding, kind, space, string(encoded)).Scan(&count)
	return count, err
}

// publishedNoteProcessingCoverage keeps collection-sized state in SQL. Only a
// bounded page of build/profile representatives crosses into Go for validation.
func (s *Store) publishedNoteProcessingCoverage(ctx context.Context, scope ProcessingCoverageScope) (ProcessingCoverageSnapshot, error) {
	selector, err := normalizePublishedNoteSelector(scope.PublishedNotes)
	if err != nil {
		return ProcessingCoverageSnapshot{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return ProcessingCoverageSnapshot{}, fmt.Errorf("starting processing coverage snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, profileErr := loadProcessingProfile(ctx, tx, scope.ProcessingProfileFingerprint)
	if profileErr != nil && !errors.Is(profileErr, ErrNotFound) {
		return ProcessingCoverageSnapshot{}, profileErr
	}
	predicate, selectedArgs := publishedNoteEligibilitySQL(selector)
	selected := `WITH selected AS (SELECT cv.version_id FROM ` + nodeFrom + ` WHERE ` + predicate + `)`
	d := metadataJSONDialect{s.postgresSchema != ""}
	cells := selected + `, cells AS (
 SELECT COALESCE(h.attachment_id,'') attachment_id, COALESCE(a.build_id,'') build_id,
 COALESCE(a.profile_fingerprint,'') profile_fingerprint,
 CASE WHEN EXISTS(SELECT 1 FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id
 WHERE w.content_version_id=x.version_id AND w.profile_fingerprint=? AND w.state='waiting'
 AND ` + d.originalFileWaiter() + ` AND j.state IN ('queued','running','retry_wait')) THEN 1 ELSE 0 END rebuilding
 FROM selected x LEFT JOIN rendition_heads h ON h.content_version_id=x.version_id AND h.profile_fingerprint=?
 LEFT JOIN rendition_attachments a ON a.attachment_id=h.attachment_id)`
	args := append(append([]any{}, selectedArgs...), scope.ProcessingProfileFingerprint, scope.ProcessingProfileFingerprint)
	result := ProcessingCoverageSnapshot{Renditions: ProcessingClassCoverage{Name: "rendition"}}
	if err := tx.QueryRowContext(ctx, cells+` SELECT COUNT(*),COALESCE(SUM(rebuilding),0) FROM cells`, args...).
		Scan(&result.Renditions.Total, &result.Renditions.Rebuilding); err != nil {
		return ProcessingCoverageSnapshot{}, err
	}
	type buildGroup struct {
		build, profile, attachment string
		total, rebuilding          int
	}
	const pageSize = 128
	lastBuild, lastProfile, first := "", "", true
	for {
		where := ""
		pageArgs := append([]any{}, args...)
		if !first {
			where = ` WHERE build_id>? OR (build_id=? AND profile_fingerprint>?)`
			pageArgs = append(pageArgs, lastBuild, lastBuild, lastProfile)
		}
		rows, err := tx.QueryContext(ctx, cells+` SELECT build_id,profile_fingerprint,MIN(attachment_id),COUNT(*),SUM(rebuilding)
 FROM cells`+where+` GROUP BY build_id,profile_fingerprint ORDER BY build_id,profile_fingerprint LIMIT 128`, pageArgs...)
		if err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		page := make([]buildGroup, 0, pageSize)
		for rows.Next() {
			var group buildGroup
			if err := rows.Scan(&group.build, &group.profile, &group.attachment, &group.total, &group.rebuilding); err != nil {
				_ = rows.Close()
				return ProcessingCoverageSnapshot{}, err
			}
			page = append(page, group)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		// Close rows before validation queries so PostgreSQL uses no overlapping
		// readers on the transaction connection.
		for _, group := range page {
			complete, err := processingRenditionAvailableTx(ctx, tx, group.attachment)
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			if complete {
				result.Renditions.Complete += group.total - group.rebuilding
				result.Renditions.PreviousGenerationServing += group.rebuilding
			}
		}
		if len(page) < pageSize {
			break
		}
		last := page[len(page)-1]
		lastBuild, lastProfile, first = last.build, last.profile, false
	}
	result.Renditions.Unavailable = result.Renditions.Total - result.Renditions.Complete - result.Renditions.Rebuilding
	result.Renditions.State = processingCoverageClassState(result.Renditions)
	for _, requested := range scope.Bindings {
		item := ProcessingClassCoverage{Name: requested.BindingID, Required: requested.Required,
			Total: result.Renditions.Total, Unavailable: result.Renditions.Total}
		if profileErr == nil {
			binding, fingerprints, err := embeddingProfileBindingAuthority(ctx, tx, scope.ProcessingProfileFingerprint, requested.BindingID)
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			if (binding.Activation == document.EmbeddingRequired) != requested.Required {
				return ProcessingCoverageSnapshot{}, errors.New("coverage binding does not match processing profile authority")
			}
			space := fingerprints.VectorSpace[binding.Name]
			if requested.VectorSpaceID != "" {
				space = requested.VectorSpaceID
			}
			query := selected + `, complete_versions AS (SELECT DISTINCT cv.version_id ` + currentEmbeddingHeadsSQL() + `
 AND eh.profile_fingerprint=? AND eh.binding_id=? AND eh.input_kind=? AND eh.vector_space_id=?
 AND cv.version_id IN (SELECT version_id FROM selected)), rebuilding_versions AS (
 SELECT DISTINCT cv.version_id FROM ` + nodeFrom + `
 JOIN embedding_jobs j ON j.content_version_id=cv.version_id
 JOIN embedding_input_generations g ON g.generation_id=j.generation_id
 WHERE n.kind='file' AND n.trashed_at IS NULL AND j.profile_fingerprint=? AND j.binding_id=?
 AND j.input_kind=? AND j.vector_space_id=? AND j.state IN ('queued','running','retry_wait')
 AND (j.input_kind='original_file' OR (j.input_kind='image' AND ` + currentImageEmbeddingSQL("j.content_version_id", "g.evidence_fingerprint") + `) OR (j.input_kind='rendition_chunk' AND EXISTS (
 SELECT 1 FROM rendition_heads h WHERE h.content_version_id=j.content_version_id
 AND h.profile_fingerprint=j.profile_fingerprint AND h.attachment_id=g.attachment_id)))
 AND cv.version_id IN (SELECT version_id FROM selected))
 SELECT (SELECT COUNT(*) FROM complete_versions c WHERE NOT EXISTS (SELECT 1 FROM rebuilding_versions r WHERE r.version_id=c.version_id)),
 (SELECT COUNT(*) FROM rebuilding_versions),
 (SELECT COUNT(*) FROM complete_versions c JOIN rebuilding_versions r ON r.version_id=c.version_id)`
			queryArgs := append([]any{}, selectedArgs...)
			for range 2 {
				queryArgs = append(queryArgs, scope.ProcessingProfileFingerprint, binding.Name, binding.InputKind, space)
			}
			if err := tx.QueryRowContext(ctx, query, queryArgs...).Scan(&item.Complete, &item.Rebuilding, &item.PreviousGenerationServing); err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
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
