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
	"slices"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/upload"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

var errPhotoDescriptionInput = errors.New("photo preview input unavailable")

// profileRenditionInput names what a profile's provider receives: a derived
// preview for photo description, the original file otherwise.
func profileRenditionInput(profile configuredProfile) document.RenditionInputKind {
	if profile.portable.Rendition != nil && profile.portable.Rendition.AdapterContract == document.VisionAdapterContract {
		return document.RenditionInputDerivedUpload
	}
	return document.RenditionInputOriginalFile
}

func (service *Service) inspectPlannedRendition(ctx context.Context, node store.Node, version store.ContentVersion, profile configuredProfile) (media.CapabilityRecord, error) {
	if profileRenditionInput(profile) == document.RenditionInputDerivedUpload {
		return service.planPhotoDescription(ctx, version, profile)
	}
	return inspectRenditionSource(ctx, service.blobs, node, version, profile)
}

// planPhotoDescription reports whether a start could describe the version's
// photo, without reading its preview.
func (service *Service) planPhotoDescription(ctx context.Context, version store.ContentVersion, profile configuredProfile) (media.CapabilityRecord, error) {
	eligible, err := service.catalog.PhotoDescriptionEligible(ctx, version.ID)
	if err != nil {
		return media.CapabilityRecord{}, err
	}
	if !eligible {
		return media.CapabilityRecord{Reason: "photo_not_eligible"}, nil
	}
	view, reason, err := photoDescriptionPreview(ctx, service.catalog, version.ID)
	if err != nil {
		return media.CapabilityRecord{}, err
	}
	switch {
	case reason != "":
		return media.CapabilityRecord{Reason: reason}, nil
	case view.Generation.Preview.Output.Size > profile.portable.Rendition.MaxDocumentBytes:
		return media.CapabilityRecord{Reason: media.CapabilityReasonSourceBytes}, nil
	}
	return media.CapabilityRecord{Eligible: true}, nil
}

// photoDescriptionPreview distinguishes preview readiness from missing source authority.
func photoDescriptionPreview(ctx context.Context, catalog *store.Store, versionID string) (store.VisualPreviewView, media.CapabilityReason, error) {
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
	if view.Generation.Preview.State != document.VisualPreviewReady || view.Generation.Preview.Output == nil {
		return view, "preview_unavailable", nil
	}
	return view, "", nil
}

// photoDescriptionBinding names the job a request admits: this version, this
// verified preview, and the kind of request.
func (service *Service) photoDescriptionBinding(ctx context.Context, version store.ContentVersion, explicit bool) (string, error) {
	view, reason, err := photoDescriptionPreview(ctx, service.catalog, version.ID)
	if err != nil {
		return "", err
	}
	if reason != "" {
		return "", fmt.Errorf("%w: %s", ErrSourceIneligible, reason)
	}
	return store.PhotoDescriptionInputBinding(version.ID, view.Generation.Checksum, explicit), nil
}

// photoDescriptionInput returns the copy of the verified grid preview that a
// provider receives, with the capability record that binds it.
func photoDescriptionInput(ctx context.Context, catalog *store.Store, blobs *blob.Store, version store.ContentVersion, profile configuredProfile) ([]byte, media.CapabilityRecord, error) {
	eligible, err := catalog.PhotoDescriptionEligible(ctx, version.ID)
	if err != nil {
		return nil, media.CapabilityRecord{}, err
	}
	if !eligible {
		return nil, media.CapabilityRecord{}, fmt.Errorf("%w: photo_not_eligible", ErrSourceIneligible)
	}
	view, reason, err := photoDescriptionPreview(ctx, catalog, version.ID)
	if err != nil {
		return nil, media.CapabilityRecord{}, err
	}
	if reason != "" {
		return nil, media.CapabilityRecord{}, fmt.Errorf("%w: %s", ErrSourceIneligible, reason)
	}
	output := view.Generation.Preview.Output
	if output.Size > profile.portable.Rendition.MaxDocumentBytes {
		return nil, media.CapabilityRecord{}, fmt.Errorf("%w: %s", ErrSourceIneligible, media.CapabilityReasonSourceBytes)
	}
	source, size, err := blobs.OpenStreamContext(ctx, output.BlobSHA256)
	if err != nil {
		return nil, media.CapabilityRecord{}, fmt.Errorf("%w: %w", errPhotoDescriptionInput, err)
	}
	if size != output.Size {
		return nil, media.CapabilityRecord{}, errors.Join(store.ErrPackageConflict, source.Close())
	}
	raw, err := io.ReadAll(io.LimitReader(source, output.Size+1))
	if err = errors.Join(err, source.Verify(), source.Close()); err != nil {
		return nil, media.CapabilityRecord{}, fmt.Errorf("%w: %w", errPhotoDescriptionInput, err)
	}
	stripped, err := stripPhotoPreview(ctx, raw)
	if err != nil {
		return nil, media.CapabilityRecord{}, err
	}
	policy := inspectionPolicy("preview.jpg", store.ContentVersion{BlobHash: output.BlobSHA256, Size: output.Size, MimeType: "image/jpeg"}, profile)
	digest := sha256.Sum256(stripped)
	policy.InputKind, policy.MaxPixels = document.RenditionInputDerivedUpload, 1024*1024
	policy.ExpectedBytes, policy.ExpectedSHA256 = int64(len(stripped)), hex.EncodeToString(digest[:])
	if int64(len(stripped)) > policy.MaxSourceBytes {
		return nil, media.CapabilityRecord{}, fmt.Errorf("%w: %s", ErrSourceIneligible, media.CapabilityReasonSourceBytes)
	}
	capability, err := media.InspectCapability(bytes.NewReader(stripped), policy)
	return stripped, capability, err
}

// stripPhotoPreview decodes a verified preview within the grid recipe's edge
// bound and encodes its pixels again with the encoder that made it. A restored
// preview can carry metadata the producer never wrote; this copy carries none.
func stripPhotoPreview(ctx context.Context, raw []byte) ([]byte, error) {
	grid, err := document.BuiltInVisualPreviewRecipe("grid")
	if err != nil {
		return nil, err
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width > grid.MaxEdgePixels || config.Height > grid.MaxEdgePixels {
		return nil, fmt.Errorf("%w: preview_unavailable", ErrSourceIneligible)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: preview_unavailable", ErrSourceIneligible)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return encodeVisualPreviewJPEG(decoded)
}

func preparePhotoDescription(ctx context.Context, catalog *store.Store, blobs *blob.Store, version store.ContentVersion, profile configuredProfile, spool string, clock func() time.Time, withUpload bool) (providerExecution, error) {
	data, capability, err := photoDescriptionInput(ctx, catalog, blobs, version, profile)
	if err != nil {
		return providerExecution{}, err
	}
	if !capability.Eligible {
		return providerExecution{}, fmt.Errorf("%w: %s", ErrSourceIneligible, capability.Reason)
	}
	empty := sha256.Sum256(nil)
	metadata := document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/jpeg", ByteLength: int64(len(data)), SHA256: capability.SourceSHA256, CapabilityRecordChecksum: capability.Checksum, ProviderMetadataChecksum: hex.EncodeToString(empty[:]), InputKind: document.RenditionInputDerivedUpload}
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

// HasPhotoDescriptionProfiles reports whether automatic photo work is configured.
func (service *Service) HasPhotoDescriptionProfiles() bool {
	return len(service.visionProfileNames()) > 0
}

func (service *Service) visionProfileNames() []string {
	var names []string
	for name, profile := range service.profiles {
		if profileRenditionInput(profile) == document.RenditionInputDerivedUpload {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// PhotoDescriptionBackfill admits bounded work for eligible photos once a
// ready profile has consent. The ordinary rendition worker runs the jobs.
func (service *Service) PhotoDescriptionBackfill(ctx context.Context, logger *slog.Logger, workers int) error {
	if !service.HasPhotoDescriptionProfiles() {
		return nil
	}
	workers = max(workers, 1)
	backfill := Backfill[photoDescriptionTarget]{Name: "photo-descriptions", Page: 50, IdleDelay: time.Second, Logger: logger,
		Key: func(value photoDescriptionTarget) string { return value.version.ID },
		List: func(ctx context.Context, after string, limit int) ([]photoDescriptionTarget, error) {
			return service.photoDescriptionTargets(ctx, after, limit, workers)
		},
		Process: service.admitPhotoDescription,
	}
	return backfill.Run(ctx)
}

// admitPhotoDescription queues one listed photo as automatic work.
func (service *Service) admitPhotoDescription(ctx context.Context, value photoDescriptionTarget) error {
	profile := service.profiles[value.profile]
	node, err := service.catalog.NodeByID(ctx, value.version.NodeID)
	if err != nil {
		return err
	}
	binding, err := service.photoDescriptionBinding(ctx, value.version, false)
	if err != nil {
		return err
	}
	_, _, err = service.enqueueRendition(ctx, node, value.version, profile, service.renditionConsentRequest(profile), binding)
	return err
}

// photoDescriptionTarget is one photo to describe with the profile that was ready when it was listed.
type photoDescriptionTarget struct {
	profile string
	version store.ContentVersion
}

// photoDescriptionTargets lists photos once fewer than workers requests wait,
// so a large library cannot flood the queue.
func (service *Service) photoDescriptionTargets(ctx context.Context, after string, limit, workers int) ([]photoDescriptionTarget, error) {
	name, err := service.photoDescriptionProfile(ctx)
	if name == "" || err != nil {
		return nil, err
	}
	profile := service.profiles[name]
	var waiting int
	for {
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
	out := make([]photoDescriptionTarget, len(versions))
	for i, version := range versions {
		out[i] = photoDescriptionTarget{profile: name, version: version}
	}
	return out, nil
}

// photoDescriptionProfile selects the first description profile, by name, that
// has consent and a provider that answers its probe.
func (service *Service) photoDescriptionProfile(ctx context.Context) (string, error) {
	for _, name := range service.visionProfileNames() {
		err := service.requireVisionReadiness(ctx, service.profiles[name])
		switch {
		case err == nil:
			return name, nil
		case errors.Is(err, store.ErrProcessingConsentRequired), errors.Is(err, store.ErrProcessingConsentExpired),
			errors.Is(err, store.ErrProcessingConsentRevoked), errors.Is(err, ErrRenditionRuntimeUnavailable):
		default:
			return "", err
		}
	}
	return "", nil
}
