package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ProcessingCoverageScope is one bounded exact-version coverage snapshot.
// Rendition and embedding cells are evaluated in the same read transaction.
type ProcessingCoverageScope struct {
	ContentVersionIDs            []string
	ProcessingProfileFingerprint string
	Bindings                     []CoverageBinding
}

// ProcessingClassCoverage reports mutually exclusive current states. A cell
// counted as rebuilding can also report that its prior complete generation is
// still the serving authority.
type ProcessingClassCoverage struct {
	State                     CoverageState
	Complete                  int
	Unavailable               int
	Stale                     int
	Ineligible                int
	Rebuilding                int
	PreviousGenerationServing int
	Total                     int
}

// ProcessingCoverageSnapshot joins rendition and embedding coverage from one
// catalog snapshot so head, live-version, and job transitions cannot mix.
type ProcessingCoverageSnapshot struct {
	Renditions ProcessingClassCoverage
	Embeddings Coverage
}

// ProcessingCoverage captures current/live eligibility, active heads, and
// relevant replacement jobs atomically for an exact source fence.
func (s *Store) ProcessingCoverage(
	ctx context.Context, scope ProcessingCoverageScope,
) (ProcessingCoverageSnapshot, error) {
	if err := validateProcessingCoverageScope(scope); err != nil {
		return ProcessingCoverageSnapshot{}, err
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
	for _, requested := range scope.Bindings {
		if errors.Is(profileErr, ErrNotFound) {
			break
		}
		binding, fingerprints, err := embeddingProfileBindingAuthority(
			ctx, tx, scope.ProcessingProfileFingerprint, requested.BindingID)
		if err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		if binding.InputKind != requested.InputKind ||
			(binding.Activation == "required") != requested.Required ||
			(requested.VectorSpaceID != "" && requested.VectorSpaceID != fingerprints.VectorSpace[binding.Name]) {
			return ProcessingCoverageSnapshot{}, errors.New("coverage binding does not match processing profile authority")
		}
	}

	result := ProcessingCoverageSnapshot{
		Renditions: ProcessingClassCoverage{Total: len(scope.ContentVersionIDs)},
		Embeddings: Coverage{State: CoverageComplete},
	}
	for _, versionID := range scope.ContentVersionIDs {
		state, previous, err := processingRenditionCoverageStateTx(
			ctx, tx, versionID, scope.ProcessingProfileFingerprint)
		if err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
		if err := countProcessingCoverageState(&result.Renditions, state, previous); err != nil {
			return ProcessingCoverageSnapshot{}, err
		}
	}
	result.Renditions.State = summarizeProcessingClassCoverage(result.Renditions)

	for _, binding := range scope.Bindings {
		item := BindingCoverage{Binding: binding, Total: len(scope.ContentVersionIDs)}
		for _, versionID := range scope.ContentVersionIDs {
			state, previous, err := processingEmbeddingCoverageStateTx(ctx, tx, versionID,
				scope.ProcessingProfileFingerprint, binding)
			if err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
			if err := countBindingCoverageState(&item, state, previous); err != nil {
				return ProcessingCoverageSnapshot{}, err
			}
		}
		item.State = summarizeBindingCoverage(item)
		if binding.Required {
			result.Embeddings.Required = append(result.Embeddings.Required, item)
		} else {
			result.Embeddings.Optional = append(result.Embeddings.Optional, item)
		}
	}
	result.Embeddings.State = summarizeCoverage(result.Embeddings)
	if err := tx.Commit(); err != nil {
		return ProcessingCoverageSnapshot{}, fmt.Errorf("closing processing coverage snapshot: %w", err)
	}
	return result, nil
}

func validateProcessingCoverageScope(scope ProcessingCoverageScope) error {
	if len(scope.ContentVersionIDs) < 1 || len(scope.ContentVersionIDs) > maxEmbeddingCorpusMembers {
		return errors.New("processing coverage scope is outside bounded counts")
	}
	if len(scope.Bindings) > maxEmbeddingCatalogRows ||
		len(scope.ContentVersionIDs) > maxEmbeddingCoverageWork/(len(scope.Bindings)+1) {
		return errors.New("processing coverage work exceeds bounded cells")
	}
	if err := validateCatalogSHA256(scope.ProcessingProfileFingerprint, "coverage profile fingerprint"); err != nil {
		return err
	}
	seenVersions := make(map[string]struct{}, len(scope.ContentVersionIDs))
	for _, versionID := range scope.ContentVersionIDs {
		if err := validateUUIDv4(versionID); err != nil {
			return fmt.Errorf("coverage content version: %w", err)
		}
		if _, duplicate := seenVersions[versionID]; duplicate {
			return errors.New("coverage scope contains duplicate content versions")
		}
		seenVersions[versionID] = struct{}{}
	}
	seenBindings := make(map[string]struct{}, len(scope.Bindings))
	for _, binding := range scope.Bindings {
		if err := validateEmbeddingCatalogText(binding.BindingID, "coverage binding ID"); err != nil {
			return err
		}
		if err := validateEmbeddingInputKind(binding.InputKind); err != nil {
			return err
		}
		if binding.VectorSpaceID != "" {
			if err := validateCatalogSHA256(binding.VectorSpaceID, "coverage vector-space ID"); err != nil {
				return err
			}
		}
		identity := binding.BindingID + "\x00" + string(binding.InputKind)
		if _, duplicate := seenBindings[identity]; duplicate {
			return errors.New("coverage scope contains duplicate bindings")
		}
		seenBindings[identity] = struct{}{}
	}
	return nil
}

func processingRenditionCoverageStateTx(
	ctx context.Context, tx *sql.Tx, versionID, profile string,
) (CoverageState, bool, error) {
	eligible, err := processingVersionEligibleTx(ctx, tx, versionID)
	if err != nil || !eligible {
		return CoverageIneligible, false, err
	}
	var buildID string
	err = tx.QueryRowContext(ctx, `SELECT a.build_id
		FROM rendition_heads h JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		 AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
		WHERE h.content_version_id=? AND h.profile_fingerprint=?`, versionID, profile).
		Scan(&buildID)
	headed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	served := false
	if headed {
		lexicalSchema, err := lexicalGenerationSchemaPresentTx(ctx, tx)
		if err != nil {
			return "", false, err
		}
		if lexicalSchema {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM rendition_lexical_heads h
				JOIN rendition_lexical_generation_builds b ON b.generation_id=h.generation_id
				WHERE h.singleton=1 AND b.build_id=?)`, buildID).Scan(&served); err != nil {
				return "", false, err
			}
		}
	}
	active, err := activeRenditionReplacementTx(ctx, tx, versionID, profile)
	if err != nil {
		return "", false, err
	}
	if active {
		return CoverageRebuilding, served, nil
	}
	if !headed {
		return CoverageUnavailable, false, nil
	}
	if !served {
		return CoverageStale, false, nil
	}
	return CoverageComplete, false, nil
}

func activeRenditionReplacementTx(
	ctx context.Context, tx *sql.Tx, versionID, profile string,
) (bool, error) {
	var state RenditionJobState
	err := tx.QueryRowContext(ctx, `SELECT j.state FROM rendition_job_waiters w
		JOIN rendition_jobs j ON j.job_id=w.job_id
		WHERE w.content_version_id=? AND w.profile_fingerprint=? AND w.state='waiting'
		ORDER BY w.updated_at DESC,w.waiter_id DESC LIMIT 1`, versionID, profile).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state == RenditionJobQueued || state == RenditionJobRunning || state == RenditionJobRetryWait, nil
}

func processingEmbeddingCoverageStateTx(
	ctx context.Context, tx *sql.Tx, versionID, profile string, binding CoverageBinding,
) (CoverageState, bool, error) {
	eligible, err := processingVersionEligibleTx(ctx, tx, versionID)
	if err != nil || !eligible {
		return CoverageIneligible, false, err
	}
	var headSetID, headSpace, headProfile string
	err = tx.QueryRowContext(ctx, `SELECT embedding_set_id,vector_space_id,profile_fingerprint
		FROM embedding_heads WHERE content_version_id=? AND binding_id=? AND input_kind=?`,
		versionID, binding.BindingID, binding.InputKind).Scan(&headSetID, &headSpace, &headProfile)
	headed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	exactHead := false
	if headed {
		var setVersion, setBinding, setKind, setSpace, setProfile string
		var attachmentID sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT s.content_version_id,s.binding_id,s.input_kind,
			s.vector_space_id,s.profile_fingerprint,g.attachment_id
			FROM embedding_sets s JOIN embedding_input_generations g ON g.generation_id=s.input_generation_id
			WHERE s.embedding_set_id=?`, headSetID).Scan(&setVersion, &setBinding, &setKind,
			&setSpace, &setProfile, &attachmentID)
		if errors.Is(err, sql.ErrNoRows) {
			return CoverageStale, false, nil
		}
		if err != nil {
			return "", false, err
		}
		exactHead = setVersion == versionID && setBinding == binding.BindingID &&
			setKind == string(binding.InputKind) && setSpace == headSpace && setProfile == headProfile &&
			headProfile == profile && (binding.VectorSpaceID == "" || headSpace == binding.VectorSpaceID)
		if exactHead && attachmentID.Valid {
			var attached bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rendition_heads
				WHERE content_version_id=? AND profile_fingerprint=? AND attachment_id=?)`,
				versionID, profile, attachmentID.String).Scan(&attached); err != nil {
				return "", false, err
			}
			exactHead = attached
		}
	}
	active, err := activeEmbeddingReplacementTx(ctx, tx, versionID, profile, binding)
	if err != nil {
		return "", false, err
	}
	if active {
		return CoverageRebuilding, exactHead, nil
	}
	if exactHead {
		return CoverageComplete, false, nil
	}
	if headed {
		return CoverageStale, false, nil
	}
	return CoverageUnavailable, false, nil
}

func activeEmbeddingReplacementTx(
	ctx context.Context, tx *sql.Tx, versionID, profile string, binding CoverageBinding,
) (bool, error) {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM embedding_jobs
		WHERE content_version_id=? AND profile_fingerprint=? AND binding_id=? AND input_kind=?
		ORDER BY updated_at DESC,job_id DESC LIMIT 1`, versionID, profile,
		binding.BindingID, binding.InputKind).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state == "queued" || state == "running" || state == "retry_wait", nil
}

func processingVersionEligibleTx(ctx context.Context, tx *sql.Tx, versionID string) (bool, error) {
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions v
		JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
		WHERE v.version_id=?)`, versionID).Scan(&eligible)
	return eligible, err
}

func countProcessingCoverageState(
	item *ProcessingClassCoverage, state CoverageState, previous bool,
) error {
	switch state {
	case CoverageComplete:
		item.Complete++
	case CoverageUnavailable:
		item.Unavailable++
	case CoverageStale:
		item.Stale++
	case CoverageIneligible:
		item.Ineligible++
	case CoverageRebuilding:
		item.Rebuilding++
	case CoveragePartial:
		return errors.New("single-version rendition coverage cannot be partial")
	}
	if previous {
		item.PreviousGenerationServing++
	}
	return nil
}

func countBindingCoverageState(item *BindingCoverage, state CoverageState, previous bool) error {
	switch state {
	case CoverageComplete:
		item.Complete++
	case CoverageUnavailable:
		item.Unavailable++
	case CoverageStale:
		item.Stale++
	case CoverageIneligible:
		item.Ineligible++
	case CoverageRebuilding:
		item.Rebuilding++
	case CoveragePartial:
		return errors.New("single-version embedding coverage cannot be partial")
	}
	if previous {
		item.PreviousGenerationServing++
	}
	return nil
}

func summarizeProcessingClassCoverage(item ProcessingClassCoverage) CoverageState {
	if item.Ineligible == item.Total {
		return CoverageIneligible
	}
	if item.Rebuilding > 0 {
		return CoverageRebuilding
	}
	if item.Complete == item.Total {
		return CoverageComplete
	}
	if item.Complete > 0 {
		return CoveragePartial
	}
	if item.Stale > 0 {
		return CoverageStale
	}
	return CoverageUnavailable
}
