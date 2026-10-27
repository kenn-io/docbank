package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
)

// EmbeddingReconcileRequest bounds one durable discovery page. Descriptors
// are process-local executable authority, never catalog intent.
type EmbeddingReconcileRequest struct {
	DescriptorFingerprints []string
	VectorSpaces           map[string]string
	After                  string
	Limit                  int
	At                     time.Time
	Descriptors            []document.EmbeddingDescriptor
	Profiles               []ProcessingProfileRecord
	PrepareGeneration      func(context.Context, string, ProcessingProfileRecord, document.EmbeddingBindingV1) (EmbeddingInputGenerationRecord, error)
	HydrateGeneration      func(context.Context, EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error)
	// Mutate guards only enqueue mutations; discovery and blob reads run outside it.
	Mutate func(context.Context, func() error) error
}

// EmbeddingReconcileResult reports work materialized from existing portable
// authority. Next is empty when the scan reached the end.
type EmbeddingReconcileResult struct {
	Next     string
	Examined int
	Enqueued int
	Skipped  int
}

type embeddingReconcileCandidate struct {
	request      EmbeddingJobRequest
	binding      document.EmbeddingBindingV1
	fingerprints document.FingerprintSet
	space        EmbeddingVectorSpaceRecord
}

// embeddingReconcilePreparation keeps one preparation attempt's own result.
type embeddingReconcilePreparation struct {
	generation EmbeddingInputGenerationRecord
	err        error
}

// ReconcileEmbeddingJobs discovers processed current versions and prepares
// configured-profile inputs under existing consent. Providers run through jobs.
func (s *Store) ReconcileEmbeddingJobs(ctx context.Context, request EmbeddingReconcileRequest) (EmbeddingReconcileResult, error) {
	if request.Limit < 1 || request.Limit > 1000 || request.At.IsZero() || (len(request.Descriptors) == 0 && len(request.DescriptorFingerprints) == 0) || request.Mutate == nil {
		return EmbeddingReconcileResult{}, errors.New("embedding reconciliation request is invalid")
	}
	executable := make(map[string]document.EmbeddingDescriptor, len(request.Descriptors))
	for _, descriptor := range request.Descriptors {
		fingerprint := descriptor.Fingerprint
		if err := validateCatalogSHA256(fingerprint, "embedding runtime descriptor fingerprint"); err != nil {
			return EmbeddingReconcileResult{}, err
		}
		executable[fingerprint] = descriptor
	}
	type targetProfile struct {
		record       ProcessingProfileRecord
		portable     document.ProcessingProfileV1
		fingerprints document.FingerprintSet
	}
	targets := make([]targetProfile, 0, len(request.Profiles))
	for _, record := range request.Profiles {
		var portable document.ProcessingProfileV1
		if err := json.Unmarshal(record.CanonicalProfile, &portable); err != nil {
			return EmbeddingReconcileResult{}, err
		}
		_, fingerprints, err := document.CanonicalProfile(portable)
		if err != nil {
			return EmbeddingReconcileResult{}, err
		}
		targets = append(targets, targetProfile{record, portable, fingerprints})
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
			sourceProfile, err := loadProcessingProfile(ctx, tx, generation.ProcessingProfileFingerprint)
			if err != nil {
				return err
			}
			localTargets := targets
			if len(request.Profiles) == 0 {
				var portable document.ProcessingProfileV1
				if err := json.Unmarshal(sourceProfile.CanonicalProfile, &portable); err != nil {
					return err
				}
				_, fingerprints, err := document.CanonicalProfile(portable)
				if err != nil {
					return err
				}
				localTargets = []targetProfile{{sourceProfile, portable, fingerprints}}
				for _, binding := range portable.Embeddings {
					if !slices.Contains(request.DescriptorFingerprints, binding.Descriptor.Fingerprint) {
						continue
					}
					space, err := loadVectorSpaceTx(ctx, tx, fingerprints.VectorSpace[binding.Name])
					if errors.Is(err, ErrNotFound) {
						continue
					}
					if err != nil {
						return err
					}
					executable[space.DescriptorFingerprint] = space.Descriptor
				}
			}
			for _, target := range localTargets {
				if !embeddingReplacementProfile(sourceProfile, target.record) ||
					(sourceProfile.Fingerprint != target.record.Fingerprint && slices.ContainsFunc(targets, func(p targetProfile) bool { return p.record.Fingerprint == sourceProfile.Fingerprint })) {
					continue
				}
				if request.PrepareGeneration == nil {
					current, err := embeddingGenerationCurrentTx(ctx, tx, generation, target.record.Fingerprint)
					if err != nil {
						return err
					}
					if !current {
						continue
					}
				}
				profile, portable, fingerprints := target.record, target.portable, target.fingerprints
				for _, binding := range portable.Embeddings {
					if (binding.InputKind == document.EmbeddingInputRenditionChunk) != (generation.GenerationBlobHash != "") {
						continue
					}
					descriptor, ok := executable[binding.Descriptor.Fingerprint]
					if !ok {
						continue
					}
					spaceID := request.VectorSpaces[profile.Fingerprint+"/"+binding.Name]
					if spaceID == "" {
						spaceID = fingerprints.VectorSpace[binding.Name]
					}
					space := EmbeddingVectorSpaceRecord{ID: spaceID, Descriptor: descriptor}
					if settled, err := embeddingReconcileSettledTx(ctx, tx, generation, sourceProfile.Fingerprint, profile.Fingerprint, binding, spaceID); err != nil {
						return err
					} else if settled {
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
						VectorSpaceID: space.ID, Descriptor: space.Descriptor, InputGeneration: generation, Authorization: consent,
					}, binding: binding, fingerprints: fingerprints, space: space})
				}
			}
			result.Examined++
		}
		return nil
	})
	if err != nil {
		return EmbeddingReconcileResult{}, err
	}
	seen := make(map[[4]string]bool)
	// Repeated candidates reuse one preparation attempt, including its error.
	prepared := make(map[[3]string]embeddingReconcilePreparation)
	for _, candidate := range candidates {
		if request.PrepareGeneration != nil {
			key := [3]string{candidate.request.ContentVersionID, candidate.request.Profile.Fingerprint, candidate.binding.Name}
			attempt, found := prepared[key]
			if !found {
				attempt.generation, attempt.err = request.PrepareGeneration(ctx, key[0], candidate.request.Profile, candidate.binding)
				prepared[key] = attempt
			}
			if attempt.err != nil || attempt.generation.ID == "" {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				result.Skipped++
				continue
			}
			candidate.request.InputGeneration = attempt.generation
		}

		if candidate.binding.InputKind == document.EmbeddingInputRenditionChunk {
			if request.HydrateGeneration == nil {
				return EmbeddingReconcileResult{}, errors.New("embedding reconciliation requires exact E2 hydration")
			}
			hydrated, err := request.HydrateGeneration(ctx, candidate.request.InputGeneration)
			if err != nil {
				if ctx.Err() != nil {
					return EmbeddingReconcileResult{}, ctx.Err()
				}
				// A damaged or temporarily unreadable candidate must not block
				// later generations. Revisit it on a subsequent discovery pass.
				result.Skipped++
				continue
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
		key := [4]string{candidate.request.ContentVersionID, candidate.request.Profile.Fingerprint, candidate.binding.Name, candidate.request.InputGeneration.ID}
		if seen[key] {
			continue
		}
		eligible, err := s.embeddingReconcileEligible(ctx, candidate)
		if err != nil {
			return result, err
		}
		if !eligible {
			continue
		}

		if err := request.Mutate(ctx, func() error {
			_, err := s.EnqueueEmbeddingJob(ctx, candidate.request)
			return err
		}); err != nil {
			// Purge can win after the discovery transaction or during hydration.
			if errors.Is(err, ErrEmbeddingJobFenced) {
				continue
			}
			return EmbeddingReconcileResult{}, fmt.Errorf("reconciling embedding job: %w", err)
		}
		seen[key] = true
		result.Enqueued++
	}
	if more && len(generationIDs) != 0 {
		result.Next = generationIDs[len(generationIDs)-1]
	}
	return result, nil
}

// A policy move replaces only the embedding runtime policy. The
// source's other processing choices must still describe the configured target.
func embeddingReplacementProfile(source, target ProcessingProfileRecord) bool {
	if source.Fingerprint == target.Fingerprint {
		return true
	}
	var before, after document.ProcessingProfileV1
	if json.Unmarshal(source.CanonicalProfile, &before) != nil || json.Unmarshal(target.CanonicalProfile, &after) != nil {
		return false
	}
	for _, profile := range []*document.ProcessingProfileV1{&before, &after} {
		for i := range profile.Embeddings {
			profile.Embeddings[i].Descriptor.Fingerprint = ""
			profile.Embeddings[i].DisclosureFingerprint = ""
			profile.Embeddings[i].AuthorizationFingerprint = ""
			profile.Embeddings[i].CredentialBinding = ""
			profile.Embeddings[i].MaxBatchItems = 0
			profile.Embeddings[i].MaxInputBytes = 0
			profile.Embeddings[i].MaxInputTokens = 0
			profile.Embeddings[i].MaxResponseBytes = 0
		}
	}
	return reflect.DeepEqual(before, after)
}

// embeddingReconcileSettledTx reports whether a purge of the source or target
// profile suppresses the binding, or a current head already satisfies it.
func embeddingReconcileSettledTx(ctx context.Context, tx *sql.Tx, generation EmbeddingInputGenerationRecord,
	source, profile string, binding document.EmbeddingBindingV1, spaceID string,
) (bool, error) {
	for _, fingerprint := range []string{source, profile} {
		suppression, err := loadEmbeddingPurgeSuppressionTx(ctx, tx,
			EmbeddingHeadKey{generation.SourceVersionID, binding.Name, binding.InputKind}, fingerprint)
		if err == nil && suppression.active {
			return true, nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return false, err
		}
	}
	return exactEmbeddingHeadExistsTx(ctx, tx, generation.SourceVersionID, profile, binding, "", spaceID)
}

func (s *Store) embeddingReconcileEligible(ctx context.Context, candidate embeddingReconcileCandidate) (bool, error) {
	request := candidate.request
	eligible := false
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		current, err := embeddingGenerationCurrentTx(ctx, tx, request.InputGeneration, request.Profile.Fingerprint)
		if err != nil || !current {
			return err
		}
		satisfied, err := exactEmbeddingHeadExistsTx(ctx, tx, request.ContentVersionID, request.Profile.Fingerprint, candidate.binding, request.InputGeneration.ID, request.VectorSpaceID)
		if err != nil || satisfied {
			return err
		}
		prior := request.Authorization.PriorAuthorization
		if prior == nil {
			return errors.New("embedding reconciliation consent lacks admission authority")
		}
		jobID := embeddingJobID(s.vaultID, request.ContentVersionID, request.Profile.Fingerprint, candidate.binding.Name, candidate.binding.InputKind, request.InputGeneration.ID, prior.GrantID, prior.ProcessingIncarnationID, prior.RevocationFence)
		var state, spaceID string
		var failure sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT state,failure_code,vector_space_id FROM embedding_jobs WHERE job_id=?`, jobID).Scan(&state, &failure, &spaceID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		eligible = errors.Is(err, sql.ErrNoRows) || state == "abandoned" || (state == "completed" && spaceID != request.VectorSpaceID) || (state == "failed" && failure.Valid && EmbeddingFailureCode(failure.String) == EmbeddingFailureAuthorization)
		return nil
	})
	return eligible, err
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
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_heads eh
 JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id
 JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
 WHERE eh.content_version_id=? AND eh.binding_id=? AND eh.input_kind=?
 AND es.profile_fingerprint=? AND (?='' OR es.input_generation_id=?) AND es.vector_space_id=?
 AND (es.input_kind='original_file' OR `+currentRenditionChunkAuthoritySQL+`))`,
		versionID, binding.Name, binding.InputKind, profile, generationID, generationID, spaceID).Scan(&exists)
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
		if authorization, err := authorizeProviderOperationTx(ctx, tx, vaultID, candidate, at); err == nil {
			candidate.PriorAuthorization = &authorization
			return candidate, true, nil
		} else if !errors.Is(err, ErrProcessingConsentRequired) && !errors.Is(err, ErrProcessingConsentExpired) && !errors.Is(err, ErrProcessingConsentRevoked) {
			return ProviderOperationAuthorizationRequest{}, false, err
		}
	}
	return ProviderOperationAuthorizationRequest{}, false, rows.Err()
}
