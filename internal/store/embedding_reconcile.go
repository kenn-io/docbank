package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/document"
)

// EmbeddingReconcileRequest bounds one durable discovery page. Descriptor
// fingerprints are process-local executable authority, never catalog intent.
type EmbeddingReconcileRequest struct {
	After                  string
	Limit                  int
	At                     time.Time
	DescriptorFingerprints []string
	HydrateGeneration      func(context.Context, EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error)
}

// EmbeddingReconcileResult reports work materialized from existing portable
// authority. Next is empty when the scan reached the end.
type EmbeddingReconcileResult struct {
	Next     string
	Examined int
	Enqueued int
}

type embeddingReconcileCandidate struct {
	request      EmbeddingJobRequest
	binding      document.EmbeddingBindingV1
	fingerprints document.FingerprintSet
	space        EmbeddingVectorSpaceRecord
}

// ReconcileEmbeddingJobs discovers only already-materialized E1/E2 or direct
// generations for current versions. It never creates input authority, grants
// consent, or contacts a provider.
func (s *Store) ReconcileEmbeddingJobs(ctx context.Context, request EmbeddingReconcileRequest) (EmbeddingReconcileResult, error) {
	if request.Limit < 1 || request.Limit > 1000 || request.At.IsZero() || len(request.DescriptorFingerprints) == 0 {
		return EmbeddingReconcileResult{}, errors.New("embedding reconciliation request is invalid")
	}
	executable := make(map[string]struct{}, len(request.DescriptorFingerprints))
	for _, fingerprint := range request.DescriptorFingerprints {
		if err := validateCatalogSHA256(fingerprint, "embedding runtime descriptor fingerprint"); err != nil {
			return EmbeddingReconcileResult{}, err
		}
		executable[fingerprint] = struct{}{}
	}
	var generationIDs []string
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		generationIDs, err = stringColumnTx(ctx, tx, "embedding reconciliation generations", `
			SELECT g.generation_id FROM embedding_input_generations g
			JOIN content_versions v ON v.version_id=g.source_version_id
			JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
			WHERE g.generation_id>? ORDER BY g.generation_id LIMIT ?`, request.After, request.Limit+1)
		return err
	})
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
				space, err := loadVectorSpaceTx(ctx, tx, fingerprints.VectorSpace[binding.Name])
				if errors.Is(err, ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				if _, ok := executable[space.Descriptor.Fingerprint]; !ok ||
					space.Descriptor.Fingerprint != binding.Descriptor.Fingerprint {
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
		if _, err := s.EnqueueEmbeddingJob(ctx, candidate.request); err != nil {
			return EmbeddingReconcileResult{}, fmt.Errorf("reconciling embedding job: %w", err)
		}
		result.Enqueued++
	}
	if more && len(generationIDs) != 0 {
		result.Next = generationIDs[len(generationIDs)-1]
	}
	return result, nil
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
