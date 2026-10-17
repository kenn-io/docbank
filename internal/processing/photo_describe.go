package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/upload"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

var errPhotoDescriptionInput = errors.New("photo preview input unavailable")

func profileRenditionInput(profile configuredProfile) document.RenditionInputKind {
	if profile.portable.Rendition != nil && profile.provider != nil {
		descriptor := profile.provider.Descriptor()
		if descriptor.ReturnsStructured {
			for _, format := range descriptor.SupportedFormats {
				if format.InputKind == document.RenditionInputDerivedUpload && format.MediaFamily == "image" {
					return document.RenditionInputDerivedUpload
				}
			}
		}
	}
	return document.RenditionInputOriginalFile
}
func (service *Service) inspectPlannedRendition(ctx context.Context, node store.Node, version store.ContentVersion, profile configuredProfile) (media.CapabilityRecord, error) {
	if profileRenditionInput(profile) != document.RenditionInputDerivedUpload {
		return inspectRenditionSource(ctx, service.blobs, node, version, profile)
	}
	_, capability, _, err := photoDescriptionInput(ctx, service.catalog, service.blobs, version, profile)
	if errors.Is(err, ErrSourceIneligible) && capability.Reason != "" {
		return capability, nil
	}
	return capability, err
}
func photoDescriptionInput(ctx context.Context, catalog *store.Store, blobs *blob.Store, version store.ContentVersion, profile configuredProfile) ([]byte, media.CapabilityRecord, string, error) {
	eligible, err := catalog.PhotoDescriptionEligible(ctx, version.ID)
	if err != nil {
		return nil, media.CapabilityRecord{}, "", err
	}
	if !eligible {
		return nil, media.CapabilityRecord{Reason: "photo_not_eligible"}, "", ErrSourceIneligible
	}
	view, reason, err := photoDescriptionPreview(ctx, catalog, version.ID)
	if err != nil {
		return nil, media.CapabilityRecord{}, "", err
	}
	if reason != "" {
		return nil, media.CapabilityRecord{Reason: media.CapabilityReason(reason)}, "", ErrSourceIneligible
	}
	// Reuse the preview processor's decoding, orientation, and color policy.
	// The larger fit product stays in memory; only the stripped 1024px JPEG leaves.
	source, size, err := blobs.OpenSeekableContext(ctx, version.BlobHash)
	if err != nil {
		return nil, media.CapabilityRecord{}, "", fmt.Errorf("%w: %w", errPhotoDescriptionInput, err)
	}
	if size != version.Size {
		return nil, media.CapabilityRecord{}, "", errors.Join(store.ErrPackageConflict, source.Close())
	}
	recipe, err := VisualPreviewRecipeForSize("fit")
	if err != nil {
		return nil, media.CapabilityRecord{}, "", errors.Join(err, source.Close())
	}
	product, err := ProduceVisualPreviewForRecipe(ctx, source, VisualPreviewTarget{SourceSHA256: version.BlobHash, Size: version.Size, MediaType: version.MimeType}, recipe)
	if err = errors.Join(err, source.Close()); err != nil {
		return nil, media.CapabilityRecord{}, "", err
	}
	if product.Preview.State != document.VisualPreviewReady {
		return nil, media.CapabilityRecord{Reason: "preview_unavailable"}, "", ErrSourceIneligible
	}
	pixels, err := jpeg.Decode(bytes.NewReader(product.Output))
	if err != nil {
		return nil, media.CapabilityRecord{}, "", fmt.Errorf("decoding normalized photo: %w", err)
	}
	scaled, err := transformPhotoPixels(ctx, pixels, 1, 1024)
	if err != nil {
		return nil, media.CapabilityRecord{}, "", err
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, scaled, &jpeg.Options{Quality: visualPreviewJPEGQuality}); err != nil {
		return nil, media.CapabilityRecord{}, "", fmt.Errorf("encoding description input: %w", err)
	}
	raw := encoded.Bytes()
	digest := sha256.Sum256(raw)
	outputHash := hex.EncodeToString(digest[:])
	if int64(len(raw)) > profile.portable.Rendition.MaxDocumentBytes {
		return nil, media.CapabilityRecord{Reason: media.CapabilityReasonSourceBytes}, "", ErrSourceIneligible
	}
	input := store.ContentVersion{BlobHash: outputHash, Size: int64(len(raw)), MimeType: "image/jpeg"}
	policy := inspectionPolicy("preview.jpg", input, profile)
	policy.InputKind = document.RenditionInputDerivedUpload
	policy.MaxPixels = 1024 * 1024
	capability, err := media.InspectCapability(bytes.NewReader(raw), policy)
	if err != nil {
		return nil, capability, "", fmt.Errorf("%w: %w", errPhotoDescriptionInput, err)
	}
	return raw, capability, view.Generation.Checksum, err
}

// photoDescriptionPreview distinguishes preview readiness from missing source authority.
func photoDescriptionPreview(ctx context.Context, catalog *store.Store, versionID string) (store.VisualPreviewView, string, error) {
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return store.VisualPreviewView{}, "", err
	}
	view, err := catalog.ContentVersionVisualPreviewByRecipe(ctx, versionID, fingerprints.GridRecipe)
	if errors.Is(err, store.ErrNotFound) {
		return view, "preview_missing", nil
	}
	if err != nil {
		return view, "", err
	}
	if view.Generation.Preview.State == document.VisualPreviewReady && view.Generation.Preview.Output != nil {
		return view, "", nil
	}
	return view, "preview_unavailable", nil
}

func preparePhotoDescription(ctx context.Context, catalog *store.Store, blobs *blob.Store, version store.ContentVersion, profile configuredProfile, spool string, clock func() time.Time, withUpload bool) (providerExecution, error) {
	data, capability, binding, err := photoDescriptionInput(ctx, catalog, blobs, version, profile)
	if err != nil {
		return providerExecution{}, err
	}
	if !capability.Eligible {
		return providerExecution{}, ErrSourceIneligible
	}
	empty := sha256.Sum256(nil)
	metadata := document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/jpeg", ByteLength: int64(len(data)), SHA256: capability.SourceSHA256, CapabilityRecordChecksum: capability.Checksum, ProviderMetadataChecksum: hex.EncodeToString(empty[:]), InputKind: document.RenditionInputDerivedUpload, InputBinding: binding}
	evidence, rendition, err := document.RenditionExecutionPoliciesForProfileV1(profile.portable)
	if err != nil {
		return providerExecution{}, err
	}
	descriptor := profile.provider.Descriptor()
	now := clock().UTC()
	maximum := int(profile.portable.Rendition.MaxResponseBytes)
	authorization := document.RenditionAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint, RenditionRequestFingerprint: profile.record.RenditionRequestFingerprint, SourceSHA256: version.BlobHash, SourceBytes: version.Size, CapabilityRecordChecksum: capability.Checksum, ProviderMetadataChecksum: metadata.ProviderMetadataChecksum, MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputDerivedUpload, AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, MaxArtifactBytes: maximum, MaxArtifacts: 1, MaxTotalResultBytes: maximum, AuthorizedAt: now.Format(timestampForm), ExpiresAt: now.Add(10 * time.Minute).Format(timestampForm)}
	result := providerExecution{Provider: profile.provider, Authorization: authorization, EvidencePolicy: evidence, RenditionPolicy: rendition, metadata: metadata}
	if withUpload {
		sealed, err := upload.Authorize(ctx, upload.Source{Reader: io.NopCloser(bytes.NewReader(data)), Directory: spool}, capability, upload.UploadMetadata{Filename: "preview.jpg"})
		if err != nil {
			return providerExecution{}, err
		}
		result.Upload = &boundAuthorizedUpload{AuthorizedUpload: sealed, metadata: metadata}
	}
	return result, nil
}

// PhotoDescriptionBackfill admits bounded work after consent and preview checks.
// The ordinary rendition worker owns execution.
func (service *Service) PhotoDescriptionBackfill(ctx context.Context, logger *slog.Logger, workers int) error {
	if !service.HasPhotoDescriptionProfiles() {
		return nil
	}
	backfill := service.photoDescriptionBackfill(logger, workers)
	return backfill.Run(ctx)
}

type photoDescriptionTarget struct {
	profile string
	version store.ContentVersion
}

func (service *Service) photoDescriptionBackfill(logger *slog.Logger, workers int) Backfill[photoDescriptionTarget] {
	var name string
	for key, profile := range service.profiles {
		if profileRenditionInput(profile) == document.RenditionInputDerivedUpload {
			name = key
			break
		}
	}
	workers = max(workers, 1)
	return Backfill[photoDescriptionTarget]{Name: "photo-descriptions", Page: 50, IdleDelay: time.Second, MaxIdleDelay: time.Minute, Logger: logger,
		Key: func(value photoDescriptionTarget) string { return value.version.ID },
		List: func(ctx context.Context, after string, limit int) ([]photoDescriptionTarget, error) {
			profile := service.profiles[name]
			if _, err := service.catalog.AuthorizeProviderOperation(ctx, service.renditionConsentRequest(profile)); err != nil {
				if errors.Is(err, store.ErrProcessingConsentRequired) || errors.Is(err, store.ErrProcessingConsentExpired) || errors.Is(err, store.ErrProcessingConsentRevoked) {
					return nil, nil
				}
				return nil, err
			}
			var waiting int
			for {
				var err error
				waiting, err = service.catalog.UnclaimedPhotoDescriptionWaiters(ctx, profile.record.Fingerprint, service.clock())
				if err != nil {
					return nil, err
				}
				if waiting < workers {
					break
				}
				if err := jobs.Wait(ctx, time.Second); err != nil {
					return nil, err
				}
			}
			versions, err := service.catalog.PhotoDescriptionTargets(ctx, profile.record.Fingerprint, after, min(limit, workers-waiting))
			if err != nil {
				return nil, err
			}
			out := make([]photoDescriptionTarget, 0, len(versions))
			for _, version := range versions {
				out = append(out, photoDescriptionTarget{name, version})
			}
			return out, nil
		},
		Process: func(ctx context.Context, value photoDescriptionTarget) error {
			profile := service.profiles[value.profile]
			node, err := service.catalog.NodeByID(ctx, value.version.NodeID)
			if err != nil {
				return err
			}
			_, _, err = service.enqueueRendition(ctx, node, value.version, profile, service.renditionConsentRequest(profile), "")
			return err
		},
	}
}

// HasPhotoDescriptionProfiles reports whether automatic photo work is configured.
func (service *Service) HasPhotoDescriptionProfiles() bool {
	for _, profile := range service.profiles {
		if profileRenditionInput(profile) == document.RenditionInputDerivedUpload {
			return true
		}
	}
	return false
}
