package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
)

// ImagePreparationError distinguishes retryable input/runtime failures from a
// rejection of bytes verified against the named preview.
type ImagePreparationError struct {
	EvidenceFingerprint string
	Rejected            bool
	Err                 error
}

func (e *ImagePreparationError) Error() string { return "preparing image embedding: " + e.Err.Error() }
func (e *ImagePreparationError) Unwrap() error { return e.Err }

// ImagePreparationRejectionKey identifies verified preview bytes rejected by a worker.
type ImagePreparationRejectionKey struct {
	ProfileFingerprint string
	BindingID          string
	ContentVersionID   string
	PreviewChecksum    string
}

// ImageReconcilePosition is the last profile/version examined by discovery.
type ImageReconcilePosition struct {
	ProfileFingerprint string
	ContentVersionID   string
}

// EmbeddingReconcileRequest bounds one durable discovery page. Descriptor
// fingerprints are process-local executable authority, never catalog intent.
type EmbeddingReconcileRequest struct {
	SkipGenerations        bool
	SkipImages             bool
	ImageRejections        map[ImagePreparationRejectionKey]struct{}
	ImageProfiles          []ProcessingProfileRecord
	ImageAfter             ImageReconcilePosition
	PrepareImageGeneration func(context.Context, ProcessingProfileRecord, document.EmbeddingBindingV1, string) (EmbeddingInputGenerationRecord, document.EmbeddingDescriptor, error)
	After                  string
	Limit                  int
	At                     time.Time
	DescriptorFingerprints []string
	HydrateGeneration      func(context.Context, EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error)
	// Mutate guards catalog writes; discovery and blob reads run outside it.
	Mutate func(context.Context, func() error) error
}

// EmbeddingReconcileResult reports work materialized from existing portable
// authority. Next is empty when the scan reached the end.
type EmbeddingReconcileResult struct {
	ImageNext ImageReconcilePosition
	Next      string
	Examined  int
	Enqueued  int
	Skipped   int
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
	if request.Limit < 1 || request.Limit > 1000 || request.At.IsZero() || len(request.DescriptorFingerprints) == 0 || request.Mutate == nil {
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
		if request.SkipGenerations {
			return nil
		}
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
	if !request.SkipImages && request.PrepareImageGeneration != nil {
		var err error
		result.ImageNext, err = s.discoverImageEmbeddingJobs(ctx, request, executable)
		if err != nil {
			return result, err
		}
	}
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
			// Image and original-file inputs both omit a generation blob. Their jobs
			// and sets retain the input kind needed to keep these bindings separate.
			var imageGeneration bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_jobs WHERE generation_id=? AND input_kind='image') OR EXISTS(SELECT 1 FROM embedding_sets WHERE input_generation_id=? AND input_kind='image')`, generation.ID, generation.ID).Scan(&imageGeneration); err != nil {
				return err
			}
			for _, binding := range portable.Embeddings {
				if (binding.InputKind == document.EmbeddingInputRenditionChunk) != (generation.GenerationBlobHash != "") || (binding.InputKind == document.EmbeddingInputOriginalFile && imageGeneration) {
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
				if binding.InputKind == document.EmbeddingInputImage {
					if err := validateImageEmbeddingGenerationTx(ctx, tx, generation, binding.Name); err != nil {
						continue
					}
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
				suppression, err := loadEmbeddingPurgeSuppressionTx(ctx, tx,
					EmbeddingHeadKey{generation.SourceVersionID, binding.Name, binding.InputKind}, profile.Fingerprint)
				if err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
				if err == nil && suppression.active {
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
				prior := consent.PriorAuthorization
				if prior == nil {
					return errors.New("embedding reconciliation consent lacks admission authority")
				}
				jobID := embeddingJobID(s.vaultID, generation.SourceVersionID, profile.Fingerprint,
					binding.Name, binding.InputKind, generation.ID, prior.GrantID,
					prior.ProcessingIncarnationID, prior.RevocationFence)
				var state string
				var failure sql.NullString
				err = tx.QueryRowContext(ctx, `SELECT state,failure_code
					FROM embedding_jobs WHERE job_id=?`, jobID).Scan(&state, &failure)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				recoverableAuthorization := state == "failed" && failure.Valid &&
					EmbeddingFailureCode(failure.String) == EmbeddingFailureAuthorization
				if err == nil && state != "abandoned" && !recoverableAuthorization {
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
		if authorization, err := authorizeProviderOperationTx(ctx, tx, vaultID, candidate, at); err == nil {
			candidate.PriorAuthorization = &authorization
			return candidate, true, nil
		} else if !errors.Is(err, ErrProcessingConsentRequired) && !errors.Is(err, ErrProcessingConsentExpired) && !errors.Is(err, ErrProcessingConsentRevoked) {
			return ProviderOperationAuthorizationRequest{}, false, err
		}
	}
	return ProviderOperationAuthorizationRequest{}, false, rows.Err()
}

func (s *Store) discoverImageEmbeddingJobs(ctx context.Context, request EmbeddingReconcileRequest, executable map[string]struct{}) (ImageReconcilePosition, error) {
	type target struct {
		version  string
		profile  ProcessingProfileRecord
		binding  document.EmbeddingBindingV1
		checksum string
		consent  ProviderOperationAuthorizationRequest
	}
	quality, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return ImageReconcilePosition{}, err
	}
	profiles := map[string]ProcessingProfileRecord{}
	for _, profile := range request.ImageProfiles {
		profiles[profile.Fingerprint] = profile
	}
	var targets []target
	type pair struct{ profile, version string }
	var pairs []pair
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		names := slices.Sorted(maps.Keys(profiles))
		for _, name := range names {
			if name < request.ImageAfter.ProfileFingerprint {
				continue
			}
			var portable document.ProcessingProfileV1
			if err := json.Unmarshal(profiles[name].CanonicalProfile, &portable); err != nil {
				return err
			}
			if !slices.ContainsFunc(portable.Embeddings, func(b document.EmbeddingBindingV1) bool {
				_, ok := executable[b.Descriptor.Fingerprint]
				return b.InputKind == document.EmbeddingInputImage && ok
			}) {
				continue
			}
			after := ""
			if name == request.ImageAfter.ProfileFingerprint {
				after = request.ImageAfter.ContentVersionID
			}
			versions, err := stringColumnTx(ctx, tx, "image reconciliation versions", `SELECT v.version_id FROM content_versions v JOIN nodes n ON n.id=v.node_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.version_id>? AND n.current_version_id=v.version_id AND a.display_file_id=f.file_id AND `+photoDescriptionEligibility+` ORDER BY v.version_id LIMIT ?`, after, request.Limit+1-len(pairs))
			if err != nil {
				return err
			}
			for _, version := range versions {
				pairs = append(pairs, pair{name, version})
			}
			if len(pairs) > request.Limit {
				break
			}
		}
		for i, p := range pairs {
			if i == request.Limit {
				break
			}
			profile := profiles[p.profile]
			var portable document.ProcessingProfileV1
			if err := json.Unmarshal(profile.CanonicalProfile, &portable); err != nil {
				return err
			}
			for _, binding := range portable.Embeddings {
				if binding.InputKind != document.EmbeddingInputImage {
					continue
				}
				if _, ok := executable[binding.Descriptor.Fingerprint]; !ok {
					continue
				}
				missing, err := imageEmbeddingMissingTx(ctx, tx, p.version, profile.Fingerprint, binding, quality.GridRecipe)
				if err != nil {
					return err
				}
				if !missing {
					continue
				}
				preview, err := visualPreviewGenerationByRecipeTx(ctx, tx, p.version, quality.GridRecipe)
				if err != nil {
					return err
				}
				key := ImagePreparationRejectionKey{profile.Fingerprint, binding.Name, p.version, preview.Checksum}
				if _, rejected := request.ImageRejections[key]; rejected {
					continue
				}
				consent, found, err := embeddingReconcileConsentTx(ctx, tx, s.vaultID, profile.Fingerprint, binding.DisclosureFingerprint, binding.InputKind, request.At)
				if err != nil {
					return err
				}
				if found {
					targets = append(targets, target{version: p.version, profile: profile, binding: binding, checksum: preview.Checksum, consent: consent})
				}
			}
		}
		return nil
	})
	if err != nil {
		return ImageReconcilePosition{}, err
	}
	for _, target := range targets {
		generation, descriptor, err := request.PrepareImageGeneration(ctx, target.profile, target.binding, target.version)
		if err != nil {
			if ctx.Err() != nil {
				return ImageReconcilePosition{}, ctx.Err()
			}
			var preparation *ImagePreparationError
			if !errors.As(err, &preparation) {
				return ImageReconcilePosition{}, err
			}
			if preparation.Rejected && preparation.EvidenceFingerprint == target.checksum && request.ImageRejections != nil {
				if len(request.ImageRejections) >= 1024 {
					clear(request.ImageRejections)
				}
				request.ImageRejections[ImagePreparationRejectionKey{target.profile.Fingerprint, target.binding.Name, target.version, target.checksum}] = struct{}{}
			}
			continue
		}
		err = request.Mutate(ctx, func() error {
			_, err := s.EnqueueEmbeddingJob(ctx, EmbeddingJobRequest{ContentVersionID: target.version, Profile: target.profile, BindingID: target.binding.Name, Descriptor: descriptor, InputGeneration: generation, Authorization: target.consent})
			return err
		})
		if err != nil && !errors.Is(err, ErrEmbeddingJobFenced) && !errors.Is(err, ErrProcessingConsentRevoked) && !errors.Is(err, ErrProcessingConsentExpired) && !errors.Is(err, ErrProcessingConsentRequired) {
			return ImageReconcilePosition{}, err
		}
	}
	if len(pairs) > request.Limit {
		p := pairs[request.Limit-1]
		return ImageReconcilePosition{ProfileFingerprint: p.profile, ContentVersionID: p.version}, nil
	}
	return ImageReconcilePosition{}, nil
}

// imageEmbeddingMissingTx reports whether the version's ready preview still
// lacks a head or job for the binding and no derivative purge suppresses it.
func imageEmbeddingMissingTx(ctx context.Context, tx *sql.Tx, version, profile string, binding document.EmbeddingBindingV1, recipe string) (bool, error) {
	var missing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM visual_preview_generations vp WHERE vp.content_version_id=? AND vp.recipe_fingerprint=? AND vp.state='ready' AND `+currentImageEmbeddingSQL("vp.content_version_id", "vp.checksum")+`
                  AND NOT EXISTS(SELECT 1 FROM embedding_heads eh JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id WHERE eh.content_version_id=vp.content_version_id AND eh.profile_fingerprint=? AND eh.binding_id=? AND eh.input_kind='image' AND eig.evidence_fingerprint=vp.checksum)
                  AND NOT EXISTS(SELECT 1 FROM embedding_jobs j JOIN embedding_input_generations eig ON eig.generation_id=j.generation_id WHERE j.content_version_id=vp.content_version_id AND j.profile_fingerprint=? AND j.binding_id=? AND j.input_kind='image' AND eig.evidence_fingerprint=vp.checksum))`, version, recipe, profile, binding.Name, profile, binding.Name).Scan(&missing); err != nil || !missing {
		return false, err
	}
	suppression, err := loadEmbeddingPurgeSuppressionTx(ctx, tx, EmbeddingHeadKey{version, binding.Name, binding.InputKind}, profile)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	return err == nil && !suppression.active, err
}

func validateImageEmbeddingGeneration(generation EmbeddingInputGenerationRecord, bindingID string) error {
	if generation.ChunkPolicyFingerprint != ImageEmbeddingPolicyFingerprint() {
		return errors.New("unknown image embedding producer policy")
	}
	if generation.ID != ImageEmbeddingGenerationID(generation.SourceVersionID, generation.ProcessingProfileFingerprint, bindingID, generation.EvidenceFingerprint) {
		return errors.New("image generation does not match exact processing profile binding identity")
	}
	return nil
}

func validateImageEmbeddingGenerationTx(ctx context.Context, tx *sql.Tx, generation EmbeddingInputGenerationRecord, bindingID string) error {
	if err := validateImageEmbeddingGeneration(generation, bindingID); err != nil {
		return err
	}
	eligible, err := photoDescriptionEligibleTx(ctx, tx, generation.SourceVersionID)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrEmbeddingJobFenced
	}
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return err
	}
	preview, err := visualPreviewGenerationByRecipeTx(ctx, tx, generation.SourceVersionID, fingerprints.GridRecipe)
	if err != nil {
		return err
	}
	if preview.Preview.State != document.VisualPreviewReady || generation.EvidenceFingerprint != preview.Checksum || generation.AttachmentID != "" || generation.GenerationBlobHash != "" || len(generation.Inputs) != 1 || generation.Inputs[0] != (EmbeddingInputReference{ID: generation.SourceVersionID, RenderedChecksum: generation.GenerationChecksum}) {
		return ErrEmbeddingJobFenced
	}
	return nil
}
