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
	"go.kenn.io/docbank/document/media"
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

// ImageReconcilePosition is the last profile/version examined by media discovery.
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
	AutomaticVideoBindings map[string]bool
	ImageAfter             ImageReconcilePosition
	PrepareImageGeneration func(context.Context, ProcessingProfileRecord, document.EmbeddingBindingV1, string) (EmbeddingInputGenerationRecord, document.EmbeddingDescriptor, error)
	After                  string
	Limit                  int
	At                     time.Time
	DescriptorFingerprints []string
	VectorSpaces           map[string]EmbeddingVectorSpaceRecord
	HydrateGeneration      func(context.Context, EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error)
	// Mutate guards catalog writes; discovery and blob reads run outside it.
	Mutate func(context.Context, func() error) error
}

// EmbeddingReconcileResult reports work materialized from existing portable
// authority. Next is empty when the scan reached the end.
type EmbeddingReconcileResult struct {
	ConsentExpiresAt time.Time
	ImageNext        ImageReconcilePosition
	Next             string
	Examined         int
	Enqueued         int
	Skipped          int
}

type embeddingReconcileCandidate struct {
	request      EmbeddingJobRequest
	binding      document.EmbeddingBindingV1
	fingerprints document.FingerprintSet
	space        EmbeddingVectorSpaceRecord
}

// ReconcileEmbeddingJobs recovers retained generations and discovers eligible
// media inputs for executable bindings. It uses existing consent and never
// contacts a provider.
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
			WHERE g.generation_id>$1 ORDER BY g.generation_id LIMIT $2`, request.After, request.Limit+1)
		return err
	})
	if err != nil {
		return EmbeddingReconcileResult{}, err
	}
	result := EmbeddingReconcileResult{}
	if !request.SkipImages && (request.PrepareImageGeneration != nil || len(request.AutomaticVideoBindings) != 0) {
		var err error
		result.ImageNext, err = s.discoverMediaEmbeddingJobs(ctx, request, executable, &result.ConsentExpiresAt)
		if err != nil {
			return result, err
		}
	}
	more := len(generationIDs) > request.Limit
	if more {
		generationIDs = generationIDs[:request.Limit]
	}
	type cachedProfile struct {
		record       ProcessingProfileRecord
		portable     document.ProcessingProfileV1
		fingerprints document.FingerprintSet
	}
	profiles := make(map[string]cachedProfile)
	var candidates []embeddingReconcileCandidate
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for _, generationID := range generationIDs {
			generation, err := loadInputGenerationTx(ctx, tx, generationID)
			if err != nil {
				return err
			}
			cached, ok := profiles[generation.ProcessingProfileFingerprint]
			if !ok {
				cached.record, err = loadProcessingProfile(ctx, tx, generation.ProcessingProfileFingerprint)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(cached.record.CanonicalProfile, &cached.portable); err != nil {
					return err
				}
				_, cached.fingerprints, err = document.CanonicalProfile(cached.portable)
				if err != nil {
					return err
				}
				profiles[generation.ProcessingProfileFingerprint] = cached
			}
			profile, portable, fingerprints := cached.record, cached.portable, cached.fingerprints
			// Image generations carry the image producer policy; original-file generations never do.
			imageGeneration := generation.ChunkPolicyFingerprint == ImageEmbeddingPolicyFingerprint()
			for _, binding := range portable.Embeddings {
				if (binding.InputKind == document.EmbeddingInputRenditionChunk) != (generation.GenerationBlobHash != "") || (binding.InputKind == document.EmbeddingInputOriginalFile && imageGeneration) {
					continue
				}
				selected := request.VectorSpaces[profile.Fingerprint+"/"+binding.Name]
				spaceID := selected.ID
				if spaceID == "" {
					spaceID = fingerprints.VectorSpace[binding.Name]
				}
				space, err := loadVectorSpaceTx(ctx, tx, spaceID)
				if errors.Is(err, ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				descriptor := space.Descriptor
				if selected.Descriptor.Fingerprint != "" {
					if !sameEmbeddingVectorSemantics(descriptor, selected.Descriptor) {
						continue
					}
					descriptor = selected.Descriptor
				}
				if _, ok := executable[descriptor.Fingerprint]; !ok ||
					descriptor.Fingerprint != binding.Descriptor.Fingerprint {
					continue
				}
				if binding.InputKind == document.EmbeddingInputImage {
					if err := validateImageEmbeddingGenerationTx(ctx, tx, generation, binding.Name); err != nil {
						continue
					}
				}
				automatic := generation.IsAutomaticVideo()
				if automatic {
					if !request.AutomaticVideoBindings[profile.Fingerprint+"/"+binding.Name] ||
						validateAutomaticVideoBinding(generation, binding, fingerprints.EmbeddingInput[binding.Name]) != nil {
						continue
					}
					if err := validateAutomaticVideoEligibilityTx(ctx, tx, generation.SourceVersionID, binding.MaxInputBytes); errors.Is(err, ErrEmbeddingJobFenced) {
						continue
					} else if err != nil {
						return err
					}
				}
				eligible, err := embeddingGenerationCurrentTx(ctx, tx, generation, profile.Fingerprint)
				if err != nil {
					return err
				}
				if !eligible {
					continue
				}
				job, err := s.reconcileEmbeddingCandidateTx(ctx, tx, generation, profile, binding, space.ID,
					descriptor, request.At, &result.ConsentExpiresAt)
				if err != nil {
					return err
				}
				if job != nil {
					candidates = append(candidates, embeddingReconcileCandidate{request: *job,
						binding: binding, fingerprints: fingerprints, space: space})
				}
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
		enqueued, err := s.enqueueReconciledEmbeddingJob(ctx, request, candidate.request)
		if err != nil {
			return EmbeddingReconcileResult{}, err
		}
		if enqueued {
			result.Enqueued++
		}
	}
	if more && len(generationIDs) != 0 {
		result.Next = generationIDs[len(generationIDs)-1]
	}
	return result, nil
}

func (s *Store) reconcileEmbeddingCandidateTx(ctx context.Context, tx *sql.Tx, generation EmbeddingInputGenerationRecord,
	profile ProcessingProfileRecord, binding document.EmbeddingBindingV1, spaceID string,
	descriptor document.EmbeddingDescriptor, at time.Time, expiry *time.Time,
) (*EmbeddingJobRequest, error) {
	satisfied, err := exactEmbeddingHeadExistsTx(ctx, tx, generation.SourceVersionID,
		profile.Fingerprint, binding, generation.ID, spaceID)
	if err != nil || satisfied {
		return nil, err
	}
	suppression, err := loadEmbeddingPurgeSuppressionTx(ctx, tx,
		EmbeddingHeadKey{generation.SourceVersionID, binding.Name, binding.InputKind}, profile.Fingerprint)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err == nil && suppression.active {
		return nil, nil
	}
	consent, found, err := embeddingReconcileConsentTx(ctx, tx, s.vaultID, profile.Fingerprint,
		binding.DisclosureFingerprint, binding.InputKind, at.UTC(), expiry)
	if err != nil || !found {
		return nil, err
	}
	prior := consent.PriorAuthorization
	jobID := embeddingJobID(s.vaultID, generation.SourceVersionID, profile.Fingerprint,
		binding.Name, binding.InputKind, generation.ID, prior.GrantID,
		prior.ProcessingIncarnationID, prior.RevocationFence)
	var state string
	var failure sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT state,failure_code FROM embedding_jobs WHERE job_id=$1`, jobID).Scan(&state, &failure)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	recoverableAuthorization := state == "failed" && failure.Valid &&
		EmbeddingFailureCode(failure.String) == EmbeddingFailureAuthorization
	if err == nil && state != "abandoned" && !recoverableAuthorization {
		return nil, nil
	}
	return &EmbeddingJobRequest{
		ContentVersionID: generation.SourceVersionID, Profile: profile, BindingID: binding.Name,
		VectorSpaceID: spaceID, Descriptor: descriptor, InputGeneration: generation, Authorization: consent,
		AutomaticVideo: generation.IsAutomaticVideo(),
	}, nil
}

func (s *Store) enqueueReconciledEmbeddingJob(ctx context.Context, request EmbeddingReconcileRequest, job EmbeddingJobRequest) (bool, error) {
	err := request.Mutate(ctx, func() error {
		_, err := s.EnqueueEmbeddingJob(ctx, job)
		return err
	})
	if errors.Is(err, ErrEmbeddingJobFenced) || errors.Is(err, ErrProcessingConsentRevoked) ||
		errors.Is(err, ErrProcessingConsentExpired) || errors.Is(err, ErrProcessingConsentRequired) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reconciling embedding job: %w", err)
	}
	return true, nil
}

func embeddingGenerationCurrentTx(ctx context.Context, tx *sql.Tx, generation EmbeddingInputGenerationRecord, profile string) (bool, error) {
	if generation.GenerationBlobHash == "" {
		return generation.AttachmentID == "", nil
	}
	var current string
	err := tx.QueryRowContext(ctx, `SELECT h.attachment_id FROM rendition_heads h
		WHERE h.content_version_id=$1 AND h.profile_fingerprint=$2`, generation.SourceVersionID, profile).Scan(&current)
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
		WHERE h.content_version_id=$1 AND h.binding_id=$2 AND h.input_kind=$3
		AND h.profile_fingerprint=$4
		AND s.profile_fingerprint=$4 AND s.input_generation_id=$5 AND s.vector_space_id=$6)`,
		versionID, binding.Name, binding.InputKind, profile, generationID, spaceID).Scan(&exists)
	return exists, err
}

func embeddingReconcileConsentTx(ctx context.Context, tx *sql.Tx, vaultID, profile, disclosure string,
	inputKind document.EmbeddingInputKind, at time.Time, expiry *time.Time,
) (ProviderOperationAuthorizationRequest, bool, error) {
	inputs, _ := json.Marshal([]string{string(inputKind)})
	retained, _ := json.Marshal([]string{"embedding_vector_set"})
	rows, err := tx.QueryContext(ctx, `SELECT g.principal,g.scope FROM processing_consent_grants g
		JOIN current_processing_incarnation c ON c.incarnation_id=g.incarnation_id
		WHERE g.vault_uid=$1 AND g.profile_fingerprint=$2 AND g.disclosure_fingerprint=$3
		AND g.input_classes_json=$4 AND g.retained_classes_json=$5
		AND (g.expires_at IS NULL OR g.expires_at>$6)
		ORDER BY g.issued_at DESC,g.grant_id DESC`, vaultID, profile, disclosure,
		string(inputs), string(retained), at.Format(timestampLayout))
	if err != nil {
		return ProviderOperationAuthorizationRequest{}, false, err
	}
	defer func() { _ = rows.Close() }()
	var candidates []ProviderOperationAuthorizationRequest
	for rows.Next() {
		var principal, scope string
		if err := rows.Scan(&principal, &scope); err != nil {
			return ProviderOperationAuthorizationRequest{}, false, err
		}
		candidate := ProviderOperationAuthorizationRequest{Principal: principal, Scope: scope,
			ProfileFingerprint: profile, DisclosureFingerprint: disclosure,
			InputClasses: []string{string(inputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}
		candidates = append(candidates, candidate)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return ProviderOperationAuthorizationRequest{}, false, err
	}
	for _, candidate := range candidates {
		if authorization, err := authorizeProviderOperationTx(ctx, tx, vaultID, candidate, at); err == nil {
			if value := authorization.expiresAt; !value.IsZero() && (expiry.IsZero() || value.Before(*expiry)) {
				*expiry = value
			}
			candidate.PriorAuthorization = &authorization
			return candidate, true, nil
		} else if !errors.Is(err, ErrProcessingConsentRequired) && !errors.Is(err, ErrProcessingConsentExpired) && !errors.Is(err, ErrProcessingConsentRevoked) {
			return ProviderOperationAuthorizationRequest{}, false, err
		}
	}
	return ProviderOperationAuthorizationRequest{}, false, nil
}

func (s *Store) discoverMediaEmbeddingJobs(ctx context.Context, request EmbeddingReconcileRequest, executable map[string]struct{}, consentExpiry *time.Time) (ImageReconcilePosition, error) {
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
	type pair struct {
		profile string
		version ContentVersion
		kind    string
	}
	var pairs []pair
	var jobs []EmbeddingJobRequest
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
			images, videoMaxBytes := false, int64(0)
			for _, binding := range portable.Embeddings {
				if _, ok := executable[binding.Descriptor.Fingerprint]; !ok {
					continue
				}
				images = images || binding.InputKind == document.EmbeddingInputImage && request.PrepareImageGeneration != nil
				if request.AutomaticVideoBindings[name+"/"+binding.Name] && binding.InputKind == document.EmbeddingInputOriginalFile {
					videoMaxBytes = max(videoMaxBytes, min(binding.MaxInputBytes, media.MaxBytes))
				}
			}
			if !images && videoMaxBytes == 0 {
				continue
			}
			after := ""
			if name == request.ImageAfter.ProfileFingerprint {
				after = request.ImageAfter.ContentVersionID
			}
			rows, err := tx.QueryContext(ctx, `SELECT v.version_id,v.blob_hash,v.size,v.recorded_at,a.kind
				FROM content_versions v JOIN nodes n ON n.id=v.node_id
				JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id
				WHERE v.version_id>$1 AND n.current_version_id=v.version_id AND a.display_file_id=f.file_id
				AND (($2 AND `+photoDescriptionEligibility+`) OR (`+videoEmbeddingEligibility+` AND v.size<=$3))
				ORDER BY v.version_id LIMIT $4`, after, images, videoMaxBytes, request.Limit+1-len(pairs))
			if err != nil {
				return err
			}
			for rows.Next() {
				p := pair{profile: name}
				if err := rows.Scan(&p.version.ID, &p.version.BlobHash, &p.version.Size, &p.version.RecordedAt, &p.kind); err != nil {
					_ = rows.Close()
					return err
				}
				pairs = append(pairs, p)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
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
			_, fingerprints, err := document.CanonicalProfile(portable)
			if err != nil {
				return err
			}
			for _, binding := range portable.Embeddings {
				if _, ok := executable[binding.Descriptor.Fingerprint]; !ok {
					continue
				}
				if p.kind == "video" {
					key := profile.Fingerprint + "/" + binding.Name
					if !request.AutomaticVideoBindings[key] || binding.InputKind != document.EmbeddingInputOriginalFile ||
						p.version.Size > binding.MaxInputBytes {
						continue
					}
					space := request.VectorSpaces[key]
					if space.ID == "" || space.Descriptor.Fingerprint != binding.Descriptor.Fingerprint {
						continue
					}
					generation := automaticVideoEmbeddingGeneration(p.version, profile.Fingerprint, fingerprints.EmbeddingInput[binding.Name], binding)
					job, err := s.reconcileEmbeddingCandidateTx(ctx, tx, generation, profile, binding, space.ID,
						space.Descriptor, request.At, consentExpiry)
					if err != nil {
						return err
					}
					if job != nil {
						jobs = append(jobs, *job)
					}
					continue
				}
				if binding.InputKind != document.EmbeddingInputImage {
					continue
				}
				missing, err := imageEmbeddingMissingTx(ctx, tx, p.version.ID, profile.Fingerprint, binding, quality.GridRecipe)
				if err != nil {
					return err
				}
				if !missing {
					continue
				}
				preview, err := visualPreviewGenerationByRecipeTx(ctx, tx, p.version.ID, quality.GridRecipe)
				if err != nil {
					return err
				}
				key := ImagePreparationRejectionKey{profile.Fingerprint, binding.Name, p.version.ID, preview.Checksum}
				if _, rejected := request.ImageRejections[key]; rejected {
					continue
				}
				consent, found, err := embeddingReconcileConsentTx(ctx, tx, s.vaultID, profile.Fingerprint, binding.DisclosureFingerprint, binding.InputKind, request.At, consentExpiry)
				if err != nil {
					return err
				}
				if found {
					targets = append(targets, target{version: p.version.ID, profile: profile, binding: binding, checksum: preview.Checksum, consent: consent})
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
		job := EmbeddingJobRequest{ContentVersionID: target.version, Profile: target.profile,
			BindingID: target.binding.Name, VectorSpaceID: request.VectorSpaces[target.profile.Fingerprint+"/"+target.binding.Name].ID,
			Descriptor: descriptor, InputGeneration: generation, Authorization: target.consent}
		if _, err := s.enqueueReconciledEmbeddingJob(ctx, request, job); err != nil {
			return ImageReconcilePosition{}, err
		}
	}
	for _, job := range jobs {
		if _, err := s.enqueueReconciledEmbeddingJob(ctx, request, job); err != nil {
			return ImageReconcilePosition{}, err
		}
	}
	if len(pairs) > request.Limit {
		p := pairs[request.Limit-1]
		return ImageReconcilePosition{ProfileFingerprint: p.profile, ContentVersionID: p.version.ID}, nil
	}
	return ImageReconcilePosition{}, nil
}

// imageEmbeddingMissingTx reports whether the version's ready preview still
// lacks a head or job for the binding and no derivative purge suppresses it.
func imageEmbeddingMissingTx(ctx context.Context, tx *sql.Tx, version, profile string, binding document.EmbeddingBindingV1, recipe string) (bool, error) {
	var missing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM visual_preview_generations vp WHERE vp.content_version_id=$1 AND vp.recipe_fingerprint=$2 AND vp.state='ready'
                  AND NOT EXISTS(SELECT 1 FROM embedding_heads eh JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id WHERE eh.content_version_id=vp.content_version_id AND eh.profile_fingerprint=$3 AND eh.binding_id=$4 AND eh.input_kind='image' AND eig.evidence_fingerprint=vp.checksum)
                  AND NOT EXISTS(SELECT 1 FROM embedding_jobs j JOIN embedding_input_generations eig ON eig.generation_id=j.generation_id WHERE j.content_version_id=vp.content_version_id AND j.profile_fingerprint=$5 AND j.binding_id=$6 AND j.input_kind='image' AND eig.evidence_fingerprint=vp.checksum))`, version, recipe, profile, binding.Name, profile, binding.Name).Scan(&missing); err != nil || !missing {
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
	return validateImageEmbeddingPreviewTx(ctx, tx, generation)
}

// imageEmbeddingPreviewEligibilitySQL checks retained preview eligibility for
// source capture. g names the generation.
func imageEmbeddingPreviewEligibilitySQL(inputs, recipe string) string {
	return `COALESCE(g.attachment_id,'')='' AND COALESCE(g.generation_blob_hash,'')='' AND g.input_count=1 AND
		EXISTS(SELECT 1 FROM ` + inputs + ` i WHERE i.generation_id=g.generation_id
			AND i.input_id=g.source_version_id AND i.rendered_checksum=g.generation_checksum) AND
		` + currentImageEmbeddingRecipeSQL("g.source_version_id", "g.evidence_fingerprint", recipe)
}

func validateImageEmbeddingPreviewTx(ctx context.Context, tx *sql.Tx, generation EmbeddingInputGenerationRecord) error {
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

// DirectEmbeddingGeneration identifies the exact original-file input to a binding.
func DirectEmbeddingGeneration(version ContentVersion, profileFingerprint,
	embeddingInputFingerprint string, binding document.EmbeddingBindingV1,
) EmbeddingInputGenerationRecord {
	id := LengthPrefixedSHA256("docbank/direct-file-input-generation/v1", version.ID, version.BlobHash,
		profileFingerprint, binding.Name, embeddingInputFingerprint)
	return EmbeddingInputGenerationRecord{ID: id, SourceVersionID: version.ID,
		ProcessingProfileFingerprint: profileFingerprint,
		EvidenceFingerprint:          LengthPrefixedSHA256("docbank/direct-file-evidence/v1", version.BlobHash),
		TokenizerFingerprint:         LengthPrefixedSHA256("docbank/direct-file-tokenizer/v1", "none"),
		ChunkPolicyFingerprint:       embeddingInputFingerprint,
		FormatterFingerprint:         LengthPrefixedSHA256("docbank/direct-file-formatter/v1", binding.DocumentFormatter, binding.ModelInput.Fingerprint),
		GenerationChecksum:           version.BlobHash,
		Inputs:                       []EmbeddingInputReference{{ID: version.ID, RenderedChecksum: version.BlobHash}},
		CreatedAt:                    version.RecordedAt}
}

const automaticVideoGenerationTag = "docbank/automatic-mp4-input-generation/v1"

var automaticVideoPolicyFingerprint = LengthPrefixedSHA256(automaticVideoGenerationTag)

// IsAutomaticVideo reports whether the retained producer policy requires
// current display eligibility and an opted-in binding at execution.
func (generation EmbeddingInputGenerationRecord) IsAutomaticVideo() bool {
	return generation.ChunkPolicyFingerprint == automaticVideoPolicyFingerprint
}

func automaticVideoEmbeddingGeneration(version ContentVersion, profile, inputFingerprint string,
	binding document.EmbeddingBindingV1,
) EmbeddingInputGenerationRecord {
	generation := DirectEmbeddingGeneration(version, profile, inputFingerprint, binding)
	generation.ID = LengthPrefixedSHA256(automaticVideoGenerationTag, generation.ID)
	generation.ChunkPolicyFingerprint = automaticVideoPolicyFingerprint
	return generation
}

func validateAutomaticVideoBinding(generation EmbeddingInputGenerationRecord, binding document.EmbeddingBindingV1, inputFingerprint string) error {
	version := ContentVersion{ID: generation.SourceVersionID, BlobHash: generation.GenerationChecksum}
	expected := automaticVideoEmbeddingGeneration(version, generation.ProcessingProfileFingerprint, inputFingerprint, binding)
	if binding.InputKind != document.EmbeddingInputOriginalFile || generation.ID != expected.ID ||
		generation.AttachmentID != "" || generation.GenerationBlobHash != "" {
		return ErrEmbeddingJobFenced
	}
	return nil
}

const videoEmbeddingEligibility = `a.kind='video' AND ` + liveIncludedDisplayPredicate +
	` AND a.hidden_at IS NULL AND v.mime_type='video/mp4' AND v.size>0`

func validateAutomaticVideoEligibilityTx(ctx context.Context, tx *sql.Tx, versionID string, maxBytes int64) error {
	var eligible bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id
		JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id
		WHERE v.version_id=$1 AND n.current_version_id=v.version_id AND a.display_file_id=f.file_id AND `+
		videoEmbeddingEligibility+` AND v.size<=$2)`, versionID, min(maxBytes, media.MaxBytes)).Scan(&eligible); err != nil {
		return err
	}
	if !eligible {
		return ErrEmbeddingJobFenced
	}
	return nil
}
