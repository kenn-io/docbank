package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
)

// EmbeddingReconcileRequest bounds one durable discovery page. Descriptor
// fingerprints are process-local executable authority, never catalog intent.
type EmbeddingReconcileRequest struct {
	After                  string
	Limit                  int
	At                     time.Time
	ProfileFingerprint     string
	ProfileFingerprints    []string
	DescriptorFingerprints []string
	// VectorSpaces carries exact runtime descriptor authority for a missing E1 record.
	VectorSpaces             map[string]EmbeddingVectorSpaceRecord
	HydrateGeneration        func(context.Context, EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error)
	GenerateOriginalFile     func(context.Context, OriginalFileGenerationRequest) (EmbeddingInputGenerationRecord, error)
	AfterRenditionAttachment string
	RenditionAttachments     []string
	SkipGenerations          bool
	SkipRenditionHeads       bool
	GenerateRenditionChunk   func(context.Context, RenditionChunkGenerationRequest) (EmbeddingInputGenerationRecord, error)
}

// OriginalFileGenerationRequest names the exact source version and profile
// binding whose direct-file input authority the processing service may rebuild.
type OriginalFileGenerationRequest struct {
	ContentVersionID   string
	ProfileFingerprint string
	BindingID          string
}

// EmbeddingReconcileResult reports work materialized from existing portable
// authority. Next is empty when the scan reached the end.
type EmbeddingReconcileResult struct {
	Next                    string
	Examined                int
	Enqueued                int
	NextRenditionAttachment string
	Generated               int
	Incomplete              bool
}

// RenditionChunkGenerationRequest names the exact published attachment whose
// E2 generation the caller must materialize. It carries no bytes and no policy.
type RenditionChunkGenerationRequest struct {
	ContentVersionID   string
	ProfileFingerprint string
	BindingID          string
	AttachmentID       string
}

type embeddingReconcileCandidate struct {
	request      EmbeddingJobRequest
	binding      document.EmbeddingBindingV1
	fingerprints document.FingerprintSet
	space        EmbeddingVectorSpaceRecord
}

type renditionChunkCandidate struct {
	request      EmbeddingJobRequest
	binding      document.EmbeddingBindingV1
	fingerprints document.FingerprintSet
	space        EmbeddingVectorSpaceRecord
	attachmentID string
}

// EnsureEmbeddingVectorSpace records the immutable descriptor needed before a
// deferred rendition chunk can be reconciled.
func (s *Store) EnsureEmbeddingVectorSpace(ctx context.Context, record EmbeddingVectorSpaceRecord) error {
	if err := validateEmbeddingVectorSpace(record); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return insertVectorSpaceTx(ctx, tx, record)
	})
}

// ReconcileEmbeddingJobs discovers materialized generations for current
// versions and asks the processing service to rebuild missing input authority
// from a current rendition head. It never grants consent or contacts a provider.
func (s *Store) ReconcileEmbeddingJobs(ctx context.Context, request EmbeddingReconcileRequest) (EmbeddingReconcileResult, error) {
	if request.Limit < 1 || request.Limit > 1000 || request.At.IsZero() || len(request.DescriptorFingerprints) == 0 {
		return EmbeddingReconcileResult{}, errors.New("embedding reconciliation request is invalid")
	}
	if request.ProfileFingerprint != "" {
		if err := validateCatalogSHA256(request.ProfileFingerprint, "processing profile fingerprint"); err != nil {
			return EmbeddingReconcileResult{}, err
		}
	}
	if request.ProfileFingerprint != "" && len(request.ProfileFingerprints) != 0 {
		return EmbeddingReconcileResult{}, errors.New("embedding reconciliation profile filters conflict")
	}
	for _, fingerprint := range request.ProfileFingerprints {
		if err := validateCatalogSHA256(fingerprint, "processing profile fingerprint"); err != nil {
			return EmbeddingReconcileResult{}, err
		}
	}
	executable := make(map[string]struct{}, len(request.DescriptorFingerprints))
	for _, fingerprint := range request.DescriptorFingerprints {
		if err := validateCatalogSHA256(fingerprint, "embedding runtime descriptor fingerprint"); err != nil {
			return EmbeddingReconcileResult{}, err
		}
		executable[fingerprint] = struct{}{}
	}
	var generationIDs []string
	var err error
	if !request.SkipGenerations {
		err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
			var err error
			query := `
			SELECT g.generation_id FROM embedding_input_generations g
			JOIN content_versions v ON v.version_id=g.source_version_id
			JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
			WHERE g.generation_id>?`
			args := []any{request.After}
			if request.ProfileFingerprint != "" {
				query += ` AND g.profile_fingerprint=?`
				args = append(args, request.ProfileFingerprint)
			} else if len(request.ProfileFingerprints) != 0 {
				query += ` AND g.profile_fingerprint IN (` + placeholders(len(request.ProfileFingerprints)) + `)`
				for _, fingerprint := range request.ProfileFingerprints {
					args = append(args, fingerprint)
				}
			}
			query += ` ORDER BY g.generation_id LIMIT ?`
			args = append(args, request.Limit+1)
			generationIDs, err = stringColumnTx(ctx, tx, "embedding reconciliation generations", query, args...)
			return err
		})
	}
	if err != nil {
		return EmbeddingReconcileResult{}, err
	}
	result := EmbeddingReconcileResult{}
	more := len(generationIDs) > request.Limit
	if more {
		generationIDs = generationIDs[:request.Limit]
	}
	var candidates []embeddingReconcileCandidate
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for _, generationID := range generationIDs {
			generation, err := loadInputGenerationTx(ctx, tx, generationID)
			if err != nil {
				return err
			}
			profile, err := loadProcessingProfile(ctx, tx, generation.ProcessingProfileFingerprint)
			if err != nil {
				return err
			}
			var portable document.ProcessingProfileV1
			if err := json.Unmarshal(profile.CanonicalProfile, &portable); err != nil {
				return err
			}
			_, fingerprints, err := document.CanonicalProfile(portable)
			if err != nil {
				return err
			}
			for _, binding := range portable.Embeddings {
				if (binding.InputKind == document.EmbeddingInputRenditionChunk) != (generation.GenerationBlobHash != "") {
					continue
				}
				if _, ok := executable[binding.Descriptor.Fingerprint]; !ok {
					continue
				}
				space, err := loadVectorSpaceTx(ctx, tx, fingerprints.VectorSpace[binding.Name])
				if errors.Is(err, ErrNotFound) {
					var found bool
					space, found = request.VectorSpaces[fingerprints.VectorSpace[binding.Name]]
					if !found {
						return errors.New("embedding reconciliation requires the exact vector-space authority")
					}
					if err := validateEmbeddingVectorSpace(space); err != nil {
						return err
					}
					if err := insertVectorSpaceTx(ctx, tx, space); err != nil {
						return err
					}
					err = nil
				}
				if err != nil {
					return err
				}
				if space.Descriptor.Fingerprint != binding.Descriptor.Fingerprint {
					continue
				}
				eligible, err := embeddingGenerationCurrentTx(ctx, tx, generation, profile.Fingerprint)
				if err != nil {
					return err
				}
				if !eligible {
					continue
				}
				satisfied, err := exactEmbeddingHeadExistsTx(ctx, tx, generation.SourceVersionID,
					profile.Fingerprint, binding, generation.ID, space.ID)
				if err != nil {
					return err
				}
				if satisfied {
					continue
				}
				consent, found, err := embeddingReconcileConsentTx(ctx, tx, s.vaultID, profile.Fingerprint,
					binding.DisclosureFingerprint, binding.InputKind, request.At.UTC())
				if err != nil {
					return err
				}
				if !found {
					result.Incomplete = true
					continue
				}
				candidates = append(candidates, embeddingReconcileCandidate{request: EmbeddingJobRequest{
					ContentVersionID: generation.SourceVersionID, Profile: profile, BindingID: binding.Name,
					Descriptor: space.Descriptor, InputGeneration: generation, Authorization: consent,
				}, binding: binding, fingerprints: fingerprints, space: space})
			}
			result.Examined++
		}
		return nil
	})
	if err != nil {
		return EmbeddingReconcileResult{}, err
	}
	for _, candidate := range candidates {
		if candidate.binding.InputKind == document.EmbeddingInputRenditionChunk {
			if request.HydrateGeneration == nil {
				return EmbeddingReconcileResult{}, errors.New("embedding reconciliation requires exact E2 hydration")
			}
			hydrated, err := request.HydrateGeneration(ctx, candidate.request.InputGeneration)
			if err != nil {
				return EmbeddingReconcileResult{}, fmt.Errorf("hydrating reconciled E2 generation: %w", err)
			}
			candidate.request.InputGeneration = hydrated
			record := EmbeddingSetRecord{BindingID: candidate.binding.Name, InputKind: candidate.binding.InputKind,
				ProcessingProfileFingerprint: candidate.request.Profile.Fingerprint,
				EmbeddingInputFingerprint:    candidate.fingerprints.EmbeddingInput[candidate.binding.Name],
				VectorSpace:                  candidate.space, InputGeneration: hydrated}
			if err := validateEmbeddingBindingAuthority(record, candidate.binding, candidate.fingerprints); err != nil {
				continue
			}
		}
		job, err := s.EnqueueEmbeddingJob(ctx, candidate.request)
		if err != nil {
			return EmbeddingReconcileResult{}, fmt.Errorf("reconciling embedding job: %w", err)
		}
		if job.Created {
			result.Enqueued++
		}
	}
	if more && len(generationIDs) != 0 {
		result.Next = generationIDs[len(generationIDs)-1]
	}
	if (request.GenerateRenditionChunk != nil || request.GenerateOriginalFile != nil) && !request.SkipRenditionHeads {
		candidates, next, incomplete, err := s.renditionEmbeddingCandidates(ctx, request, executable)
		if err != nil {
			return EmbeddingReconcileResult{}, err
		}
		result.Incomplete = result.Incomplete || incomplete
		for _, candidate := range candidates {
			var generated EmbeddingInputGenerationRecord
			if candidate.binding.InputKind == document.EmbeddingInputRenditionChunk {
				if request.GenerateRenditionChunk == nil {
					return EmbeddingReconcileResult{}, errors.New("embedding reconciliation requires rendition chunk generation authority")
				}
				generated, err = request.GenerateRenditionChunk(ctx, RenditionChunkGenerationRequest{
					ContentVersionID:   candidate.request.ContentVersionID,
					ProfileFingerprint: candidate.request.Profile.Fingerprint,
					BindingID:          candidate.binding.Name, AttachmentID: candidate.attachmentID,
				})
			} else {
				if request.GenerateOriginalFile == nil {
					return EmbeddingReconcileResult{}, errors.New("embedding reconciliation requires direct-file generation authority")
				}
				generated, err = request.GenerateOriginalFile(ctx, OriginalFileGenerationRequest{
					ContentVersionID:   candidate.request.ContentVersionID,
					ProfileFingerprint: candidate.request.Profile.Fingerprint,
					BindingID:          candidate.binding.Name,
				})
			}
			if err != nil {
				if ctx.Err() != nil {
					return EmbeddingReconcileResult{}, ctx.Err()
				}
				if failureErr := s.RecordEmbeddingFailure(ctx, EmbeddingFailureRecord{
					ContentVersionID:             candidate.request.ContentVersionID,
					ProcessingProfileFingerprint: candidate.request.Profile.Fingerprint,
					BindingID:                    candidate.binding.Name,
					InputKind:                    candidate.binding.InputKind,
					FailureCode:                  EmbeddingFailureProviderUnavailable,
					FailedAt:                     request.At.UTC().Format(timestampLayout),
				}); failureErr != nil {
					return EmbeddingReconcileResult{}, fmt.Errorf("recording embedding recovery failure: %w", failureErr)
				}
				result.Incomplete = true
				continue
			}
			if candidate.binding.InputKind == document.EmbeddingInputRenditionChunk {
				result.Generated++
			}
			candidate.request.InputGeneration = generated
			record := EmbeddingSetRecord{BindingID: candidate.binding.Name, InputKind: candidate.binding.InputKind,
				ProcessingProfileFingerprint: candidate.request.Profile.Fingerprint,
				EmbeddingInputFingerprint:    candidate.fingerprints.EmbeddingInput[candidate.binding.Name],
				VectorSpace:                  candidate.space, InputGeneration: generated}
			if candidate.binding.InputKind == document.EmbeddingInputRenditionChunk {
				if err := validateEmbeddingBindingAuthority(record, candidate.binding, candidate.fingerprints); err != nil {
					continue
				}
			}
			exists, err := s.renditionEmbeddingHeadExists(ctx, candidate, generated.ID)
			if err != nil {
				return EmbeddingReconcileResult{}, err
			}
			if exists {
				continue
			}
			job, err := s.EnqueueEmbeddingJob(ctx, candidate.request)
			if err != nil {
				return EmbeddingReconcileResult{}, fmt.Errorf("reconciling rendition embedding job: %w", err)
			}
			if job.Created {
				result.Enqueued++
			}
		}
		result.NextRenditionAttachment = next
	}
	return result, nil
}

func (s *Store) renditionEmbeddingCandidates(ctx context.Context, request EmbeddingReconcileRequest,
	executable map[string]struct{},
) ([]renditionChunkCandidate, string, bool, error) {
	var candidates []renditionChunkCandidate
	var next string
	incomplete := false
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT rh.content_version_id,rh.profile_fingerprint,rh.attachment_id
			FROM rendition_heads rh
			JOIN content_versions v ON v.version_id=rh.content_version_id
			JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id
				AND n.kind='file' AND n.trashed_at IS NULL`
		args := make([]any, 0, len(request.RenditionAttachments)+2)
		paged := len(request.RenditionAttachments) == 0
		hasProfileFilter := request.ProfileFingerprint != "" || len(request.ProfileFingerprints) != 0
		where := " WHERE "
		if request.ProfileFingerprint != "" {
			where += "rh.profile_fingerprint=?"
			args = append(args, request.ProfileFingerprint)
		} else if len(request.ProfileFingerprints) != 0 {
			where += `rh.profile_fingerprint IN (` + placeholders(len(request.ProfileFingerprints)) + `)`
			for _, fingerprint := range request.ProfileFingerprints {
				args = append(args, fingerprint)
			}
		}
		if paged {
			if hasProfileFilter {
				where += " AND "
			}
			where += "rh.attachment_id>?"
			args = append(args, request.AfterRenditionAttachment, request.Limit+1)
		} else {
			placeholders := make([]string, len(request.RenditionAttachments))
			for index, attachmentID := range request.RenditionAttachments {
				placeholders[index] = "?"
				args = append(args, attachmentID)
			}
			if hasProfileFilter {
				where += " AND "
			}
			where += `rh.attachment_id IN (` + strings.Join(placeholders, ",") + `)`
		}
		query += where + ` ORDER BY rh.attachment_id`
		if paged {
			query += ` LIMIT ?`
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		rowCount := 0
		lastPageAttachment := ""
		for rows.Next() {
			var versionID, profileFingerprint, attachmentID string
			if err := rows.Scan(&versionID, &profileFingerprint, &attachmentID); err != nil {
				return err
			}
			rowCount++
			if paged && rowCount > request.Limit {
				continue
			}
			if paged {
				lastPageAttachment = attachmentID
			}
			profile, err := loadProcessingProfile(ctx, tx, profileFingerprint)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			var portable document.ProcessingProfileV1
			if err := json.Unmarshal(profile.CanonicalProfile, &portable, json.RejectUnknownMembers(true)); err != nil {
				continue
			}
			_, fingerprints, err := document.CanonicalProfile(portable)
			if err != nil {
				continue
			}
			for _, binding := range portable.Embeddings {
				if binding.InputKind == document.EmbeddingInputRenditionChunk && request.GenerateRenditionChunk == nil {
					continue
				}
				if binding.InputKind != document.EmbeddingInputRenditionChunk &&
					(binding.InputKind != document.EmbeddingInputOriginalFile || request.GenerateOriginalFile == nil) {
					continue
				}
				if _, ok := executable[binding.Descriptor.Fingerprint]; !ok {
					continue
				}
				space, err := loadVectorSpaceTx(ctx, tx, fingerprints.VectorSpace[binding.Name])
				if errors.Is(err, ErrNotFound) {
					var found bool
					space, found = request.VectorSpaces[fingerprints.VectorSpace[binding.Name]]
					if !found {
						return errors.New("embedding reconciliation requires the exact vector-space authority")
					}
					if err := validateEmbeddingVectorSpace(space); err != nil {
						return err
					}
					if err := insertVectorSpaceTx(ctx, tx, space); err != nil {
						return err
					}
					err = nil
				}
				if err != nil {
					return err
				}
				if space.Descriptor.Fingerprint != binding.Descriptor.Fingerprint {
					continue
				}
				var jobExists bool
				var query string
				var args []any
				if binding.InputKind == document.EmbeddingInputRenditionChunk {
					query = `SELECT EXISTS(SELECT 1 FROM embedding_jobs j
						JOIN embedding_input_generations g ON g.generation_id=j.generation_id
						WHERE j.vault_uid=? AND j.content_version_id=? AND j.profile_fingerprint=?
						AND j.binding_id=? AND j.input_kind=? AND j.vector_space_id=?
						AND g.source_version_id=? AND g.profile_fingerprint=? AND g.attachment_id=?)`
					args = append(args, versionID, profileFingerprint, attachmentID)
				} else {
					query = `SELECT EXISTS(SELECT 1 FROM embedding_jobs j
						WHERE j.vault_uid=? AND j.content_version_id=? AND j.profile_fingerprint=?
						AND j.binding_id=? AND j.input_kind=? AND j.vector_space_id=?)
						OR EXISTS(SELECT 1 FROM embedding_heads h
						JOIN embedding_sets s ON s.embedding_set_id=h.embedding_set_id
						WHERE h.content_version_id=? AND h.binding_id=? AND h.input_kind=?
						AND s.profile_fingerprint=? AND s.vector_space_id=?)`
					args = append(args, versionID, binding.Name, binding.InputKind, profileFingerprint, space.ID)
				}
				if err := tx.QueryRowContext(ctx, query, args...).Scan(&jobExists); err != nil {
					return err
				}
				if jobExists {
					continue
				}
				consent, found, err := embeddingReconcileConsentTx(ctx, tx, s.vaultID,
					profileFingerprint, binding.DisclosureFingerprint, binding.InputKind, request.At.UTC())
				if err != nil {
					return err
				}
				if !found {
					incomplete = true
					continue
				}
				candidates = append(candidates, renditionChunkCandidate{
					request: EmbeddingJobRequest{ContentVersionID: versionID, Profile: profile,
						BindingID: binding.Name, Descriptor: space.Descriptor, Authorization: consent},
					binding: binding, fingerprints: fingerprints, space: space, attachmentID: attachmentID,
				})
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if paged && rowCount > request.Limit {
			next = lastPageAttachment
		}
		return nil
	})
	return candidates, next, incomplete, err
}

func (s *Store) renditionEmbeddingHeadExists(ctx context.Context, candidate renditionChunkCandidate,
	generationID string,
) (bool, error) {
	var exists bool
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		exists, err = exactEmbeddingHeadExistsTx(ctx, tx, candidate.request.ContentVersionID,
			candidate.request.Profile.Fingerprint, candidate.binding, generationID, candidate.space.ID)
		return err
	})
	return exists, err
}

func embeddingGenerationCurrentTx(ctx context.Context, tx *sql.Tx, generation EmbeddingInputGenerationRecord, profile string) (bool, error) {
	if generation.GenerationBlobHash == "" {
		return generation.AttachmentID == "", nil
	}
	var current string
	err := tx.QueryRowContext(ctx, `SELECT h.attachment_id FROM rendition_heads h
		WHERE h.content_version_id=? AND h.profile_fingerprint=?`, generation.SourceVersionID, profile).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return current == generation.AttachmentID, err
}

func exactEmbeddingHeadExistsTx(ctx context.Context, tx *sql.Tx, versionID, profile string,
	binding document.EmbeddingBindingV1, generationID, spaceID string,
) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_heads h
		JOIN embedding_sets s ON s.embedding_set_id=h.embedding_set_id
		WHERE h.content_version_id=? AND h.binding_id=? AND h.input_kind=?
		AND s.profile_fingerprint=? AND s.input_generation_id=? AND s.vector_space_id=?)`,
		versionID, binding.Name, binding.InputKind, profile, generationID, spaceID).Scan(&exists)
	return exists, err
}

func embeddingReconcileConsentTx(ctx context.Context, tx *sql.Tx, vaultID, profile, disclosure string,
	inputKind document.EmbeddingInputKind, at time.Time,
) (ProviderOperationAuthorizationRequest, bool, error) {
	inputs, _ := json.Marshal([]string{string(inputKind)})
	retained, _ := json.Marshal([]string{"embedding_vector_set"})
	rows, err := tx.QueryContext(ctx, `SELECT g.principal,g.scope FROM processing_consent_grants g
		JOIN current_processing_incarnation c ON c.incarnation_id=g.incarnation_id
		WHERE g.vault_uid=? AND g.profile_fingerprint=? AND g.disclosure_fingerprint=?
		AND g.input_classes_json=? AND g.retained_classes_json=?
		AND (g.expires_at IS NULL OR g.expires_at>?)
		ORDER BY g.issued_at DESC,g.grant_id DESC`, vaultID, profile, disclosure,
		string(inputs), string(retained), at.Format(timestampLayout))
	if err != nil {
		return ProviderOperationAuthorizationRequest{}, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var principal, scope string
		if err := rows.Scan(&principal, &scope); err != nil {
			return ProviderOperationAuthorizationRequest{}, false, err
		}
		candidate := ProviderOperationAuthorizationRequest{Principal: principal, Scope: scope,
			ProfileFingerprint: profile, DisclosureFingerprint: disclosure,
			InputClasses: []string{string(inputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}
		if _, err := authorizeProviderOperationTx(ctx, tx, vaultID, candidate, at); err == nil {
			return candidate, true, nil
		} else if !errors.Is(err, ErrProcessingConsentRequired) && !errors.Is(err, ErrProcessingConsentExpired) && !errors.Is(err, ErrProcessingConsentRevoked) {
			return ProviderOperationAuthorizationRequest{}, false, err
		}
	}
	return ProviderOperationAuthorizationRequest{}, false, rows.Err()
}
