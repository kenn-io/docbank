package processing

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func facePreview(ctx context.Context, catalog *store.Store, blobs *blob.Store, version string) ([]byte, string, []store.PhotoFace, error) {
	faces, err := catalog.FaceEmbeddingFaces(ctx, version)
	if err != nil {
		return nil, "", nil, err
	}
	quality, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return nil, "", nil, err
	}
	generation, err := catalog.VisualPreviewGenerationByRecipe(ctx, version, quality.GridRecipe)
	if err != nil {
		return nil, "", nil, err
	}
	if generation.Preview.State != document.VisualPreviewReady {
		return nil, "", nil, ErrSourceIneligible
	}
	output := generation.Preview.Output
	raw, err := verifiedPhotoPreview(ctx, blobs, output)
	if err != nil {
		return nil, generation.Checksum, nil, fmt.Errorf("%w: %w", errPhotoDescriptionInput, err)
	}
	data, err := stripPhotoPreview(ctx, raw)
	return data, generation.Checksum, faces, err
}
func (runtime *ProviderEmbeddingRuntime) faceGeneration(ctx context.Context, profile store.ProcessingProfileRecord, binding document.EmbeddingBindingV1, version string) (store.EmbeddingInputGenerationRecord, document.EmbeddingDescriptor, error) {
	blobs, ok := runtime.blobs.(*blob.Store)
	if !ok || runtime.catalog == nil {
		return store.EmbeddingInputGenerationRecord{}, document.EmbeddingDescriptor{}, ErrEmbeddingRuntimeUnavailable
	}
	data, preview, faces, err := facePreview(ctx, runtime.catalog, blobs, version)
	if err != nil {
		if errors.Is(err, errPhotoDescriptionInput) || errors.Is(err, errPhotoPreviewRejected) || errors.Is(err, ErrSourceIneligible) || errors.Is(err, store.ErrPackageConflict) {
			err = &store.ImagePreparationError{EvidenceFingerprint: preview, Rejected: errors.Is(err, errPhotoPreviewRejected), Err: err}
		}
		return store.EmbeddingInputGenerationRecord{}, document.EmbeddingDescriptor{}, err
	}
	source, err := runtime.catalog.ContentVersionByID(ctx, version)
	if err != nil {
		return store.EmbeddingInputGenerationRecord{}, document.EmbeddingDescriptor{}, err
	}
	g := store.EmbeddingInputGenerationRecord{SourceVersionID: version, ProcessingProfileFingerprint: profile.Fingerprint, EvidenceFingerprint: preview, GenerationChecksum: workerHashBytes(data), TokenizerFingerprint: store.LengthPrefixedSHA256("face/no-tokenizer"), ChunkPolicyFingerprint: store.FaceEmbeddingPolicyFingerprint(), FormatterFingerprint: store.LengthPrefixedSHA256(binding.DocumentFormatter), CreatedAt: source.RecordedAt}
	for _, face := range faces {
		g.Inputs = append(g.Inputs, store.EmbeddingInputReference{ID: face.FaceID, RenderedChecksum: store.FaceEmbeddingChecksum(g.GenerationChecksum, face)})
	}
	g.ID = store.FaceEmbeddingGenerationID(g, binding.Name)
	return g, runtime.provider.Descriptor(), nil
}
func (runtime *ProviderEmbeddingRuntime) prepareFaces(ctx context.Context, work EmbeddingWork) (EmbeddingExecution, error) {
	blobs, ok := runtime.blobs.(*blob.Store)
	if !ok || runtime.catalog == nil {
		return EmbeddingExecution{}, ErrEmbeddingRuntimeUnavailable
	}
	data, _, faces, err := facePreview(ctx, runtime.catalog, blobs, work.ContentVersionID)
	if err != nil {
		return EmbeddingExecution{}, err
	}
	var inputs []document.EmbeddingInput
	byID := make(map[string]store.PhotoFace, len(faces))
	for _, face := range faces {
		byID[face.FaceID] = face
	}
	for _, reference := range work.InputGeneration.Inputs {
		face, ok := byID[reference.ID]
		if !ok {
			return EmbeddingExecution{}, ErrEmbeddingInputRejected
		}
		inputs = append(inputs, document.EmbeddingInput{Key: face.FaceID, Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputFace, Box: new(face.Box)})
	}
	return runtime.prepareJPEG(ctx, data, directEmbeddingInspectionPolicy(work), inputs)
}
