package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

type MediaArtifactRequest struct {
	OperationID, SourceID, OccurrenceID string
	Kind, Origin, Provider, Language    string
	Filename, MediaType, SHA256         string
	ByteLength                          int64
	Content                             io.Reader
}

// PrepareMediaArtifactUpload checks replay before deriving the byte ceiling an
// HTTP artifact upload must use before it reserves staging space.
func (service *Service) PrepareMediaArtifactUpload(
	ctx context.Context, request MediaArtifactRequest,
) (MediaReceipt, int64, bool, error) {
	if service == nil {
		return MediaReceipt{}, 0, false, ErrMediaCapabilityUnavailable
	}
	if request.Kind != "media" && request.Kind != "caption" && request.Kind != "transcript" {
		return MediaReceipt{}, 0, false, errors.New("media artifact kind must be media, caption, or transcript")
	}
	if request.Origin == "" {
		request.Origin = "supplied"
	}
	if err := store.ValidateMediaInputAuthority(request.Kind, request.Origin, request.Provider,
		request.Language, request.SHA256); err != nil {
		return MediaReceipt{}, 0, false, err
	}
	if request.ByteLength < 1 || request.SHA256 == "" || request.OperationID == "" {
		return MediaReceipt{}, 0, false, errors.New("media artifact requires content and exact identity")
	}
	operation, err := service.mediaArtifactOperation(request)
	if err != nil {
		return MediaReceipt{}, 0, false, err
	}
	if replay, replayErr := service.catalog.MediaOperationReceipt(ctx, operation); replayErr == nil {
		stored, decodeErr := canonical.Decode[store.MediaPublicationReceipt]([]byte(replay))
		return mediaReceiptFromStore(stored), 0, true, decodeErr
	} else if !errors.Is(replayErr, store.ErrNotFound) {
		return MediaReceipt{}, 0, false, replayErr
	}
	current, err := service.catalog.MediaOccurrence(ctx, service.principal, request.OccurrenceID)
	if err != nil || current.SourceID != request.SourceID {
		return MediaReceipt{}, 0, false, store.ErrNotFound
	}
	limit, _, err := service.mediaArtifactLimit(current, request.Kind, request.Filename, request.MediaType)
	if err != nil {
		return MediaReceipt{}, 0, false, err
	}
	return MediaReceipt{}, limit, false, nil
}

func (service *Service) mediaArtifactOperation(request MediaArtifactRequest) (store.MediaOperation, error) {
	identity, err := canonical.Marshal(struct {
		SourceID, OccurrenceID, Kind, Origin, Provider, Language, SHA256 string
		Filename, MediaType                                              string
		ByteLength                                                       int64
	}{request.SourceID, request.OccurrenceID, request.Kind, request.Origin,
		request.Provider, request.Language, request.SHA256, request.Filename, request.MediaType,
		request.ByteLength})
	if err != nil {
		return store.MediaOperation{}, err
	}
	digest := sha256.Sum256(identity)
	return store.MediaOperation{ID: request.OperationID, Principal: service.principal,
		Verb: "import_recording_artifact", RequestSHA256: hex.EncodeToString(digest[:]), SourceID: request.SourceID}, nil
}

func (service *Service) mediaArtifactLimit(
	current store.MediaOccurrenceProjection, kind, filename, mediaType string,
) (int64, bool, error) {
	limit := service.mediaMaxBytes
	if kind != "media" || current.Kind != "remote_recording" {
		if kind != "media" {
			limit = min(limit, 16<<20)
		}
		return limit, false, nil
	}
	video, err := validateRemoteRecordingFile(filename, mediaType)
	if err != nil {
		return 0, false, err
	}
	if video {
		limit = min(limit, remoteVideoMaxBytes)
	}
	return limit, video, nil
}

func (service *Service) ImportRecordingArtifact(
	ctx context.Context, request MediaArtifactRequest,
) (MediaReceipt, error) {
	if service == nil {
		return MediaReceipt{}, ErrMediaCapabilityUnavailable
	}
	if request.Kind != "media" && request.Kind != "caption" && request.Kind != "transcript" {
		return MediaReceipt{}, errors.New("media artifact kind must be media, caption, or transcript")
	}
	if request.Origin == "" {
		request.Origin = "supplied"
	}
	if err := store.ValidateMediaInputAuthority(request.Kind, request.Origin, request.Provider,
		request.Language, request.SHA256); err != nil {
		return MediaReceipt{}, err
	}
	if request.Content == nil || request.ByteLength < 1 || request.SHA256 == "" || request.OperationID == "" {
		return MediaReceipt{}, errors.New("media artifact requires content and exact identity")
	}
	limit := service.mediaMaxBytes
	if request.Kind != "media" && limit > 16<<20 {
		limit = 16 << 20
	}
	if request.ByteLength > limit {
		return MediaReceipt{}, errors.New("byte_limit")
	}
	operation, err := service.mediaArtifactOperation(request)
	if err != nil {
		return MediaReceipt{}, err
	}
	if replay, replayErr := service.catalog.MediaOperationReceipt(ctx, operation); replayErr == nil {
		stored, decodeErr := canonical.Decode[store.MediaPublicationReceipt]([]byte(replay))
		return mediaReceiptFromStore(stored), decodeErr
	} else if !errors.Is(replayErr, store.ErrNotFound) {
		return MediaReceipt{}, replayErr
	}
	current, err := service.catalog.MediaOccurrence(ctx, service.principal, request.OccurrenceID)
	if err != nil || current.SourceID != request.SourceID {
		return MediaReceipt{}, store.ErrNotFound
	}
	limit, video, err := service.mediaArtifactLimit(current, request.Kind, request.Filename, request.MediaType)
	if err != nil {
		return MediaReceipt{}, err
	}
	if request.ByteLength > limit {
		return MediaReceipt{}, errors.New("byte_limit")
	}
	if current.Kind == "remote_recording" && request.Kind == "media" {
		return service.importRemoteRecordingMedia(ctx, request, current, operation, limit, video)
	}
	if current.SourceVersionID == "" {
		return MediaReceipt{}, store.ErrNotFound
	}
	staged, owned, err := service.mediaStagedContent(ctx, request.Content,
		request.ByteLength, limit, request.SHA256)
	if err != nil {
		return MediaReceipt{}, err
	}
	if owned {
		defer func() { _ = staged.Close() }()
	}
	ext := path.Ext(request.Filename)
	if ext == "" {
		ext = ".bin"
	}
	virtualPath := path.Join("/media-input", request.SHA256[:2], request.SHA256+strings.ToLower(ext))
	inputID := hashMediaArtifactID(request.SourceID, request.OccurrenceID, request.Kind, request.SHA256)
	var stored store.MediaPublicationReceipt
	err = service.mediaMutation(ctx, func() error {
		return service.blobs.WithMutation(ctx, func() error {
			written, err := service.blobs.WriteDetailedContext(ctx, staged)
			if err != nil {
				return err
			}
			if written.Hash != request.SHA256 || written.Size != request.ByteLength {
				return errors.New("media artifact staged identity changed during seal")
			}
			encoding, err := written.EncodingName()
			if err != nil {
				return err
			}
			stored, err = service.catalog.ImportMediaInputArtifact(ctx, store.MediaInputArtifactRequest{
				Operation: operation,
				InputID:   inputID, OccurrenceID: request.OccurrenceID, SourceVersionID: current.SourceVersionID,
				VirtualPath: virtualPath, MediaType: request.MediaType, ByteLength: written.Size,
				Physical: store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize,
					PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created},
				Kind: request.Kind, Origin: request.Origin, Provider: request.Provider,
				Language: request.Language, InputSHA: written.Hash,
			})
			return err
		})
	})
	return mediaReceiptFromStore(stored), err
}

func (service *Service) importRemoteRecordingMedia(
	ctx context.Context, request MediaArtifactRequest, current store.MediaOccurrenceProjection,
	operation store.MediaOperation, limit int64, video bool,
) (MediaReceipt, error) {
	if request.ByteLength > limit {
		return MediaReceipt{}, errors.New("byte_limit")
	}
	staged, owned, err := service.mediaStagedContent(ctx, request.Content, request.ByteLength,
		limit, request.SHA256)
	if err != nil {
		return MediaReceipt{}, err
	}
	if owned {
		defer func() { _ = staged.Close() }()
	}
	record, err := media.InspectCapability(staged, remoteRecordingInspectionPolicy(request.Filename,
		request.MediaType, request.SHA256, request.ByteLength, service.mediaMaxBytes, video))
	if err != nil {
		return MediaReceipt{}, err
	}
	if !remoteRecordingFormatAdmitted(record, video) {
		return MediaReceipt{}, fmt.Errorf("unqualified_codec: %s", record.Reason)
	}
	if err := staged.rewind(); err != nil {
		return MediaReceipt{}, err
	}
	inputID := hashMediaArtifactID(request.SourceID, request.OccurrenceID, request.Kind, request.SHA256)
	var stored store.MediaPublicationReceipt
	err = service.mediaMutation(ctx, func() error {
		return service.blobs.WithMutation(ctx, func() error {
			written, err := service.blobs.WriteDetailedContext(ctx, staged)
			if err != nil {
				return err
			}
			if written.Hash != request.SHA256 || written.Size != request.ByteLength {
				return errors.New("media artifact staged identity changed during seal")
			}
			encoding, err := written.EncodingName()
			if err != nil {
				return err
			}
			stored, err = service.catalog.RetainRemoteRecordingMedia(ctx, store.MediaInputArtifactRequest{
				Operation: operation, InputID: inputID, OccurrenceID: request.OccurrenceID,
				SourceVersionID: current.SourceVersionID,
				VirtualPath:     path.Join("/media", request.SourceID, request.SHA256+"."+record.Format),
				MediaType:       record.MediaType, ByteLength: written.Size,
				Physical: store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize,
					PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created},
				Kind: request.Kind, Origin: request.Origin, Provider: request.Provider,
				Language: request.Language, InputSHA: written.Hash,
			})
			return err
		})
	})
	return mediaReceiptFromStore(stored), err
}

func (service *Service) RetryMedia(
	ctx context.Context, operationID, sourceID string, processingRequest MediaProcessingRequest,
) (MediaReceipt, error) {
	if !mediaProcessingRequested(&processingRequest) {
		return MediaReceipt{}, errors.New("media retry requires a processing profile")
	}
	identity, err := canonical.Marshal(struct {
		SourceID   string
		Processing MediaProcessingRequest
	}{sourceID, processingRequest})
	if err != nil {
		return MediaReceipt{}, err
	}
	digest := sha256.Sum256(identity)
	operation := store.MediaOperation{ID: operationID, Principal: service.principal,
		Verb: "retry_media", RequestSHA256: hex.EncodeToString(digest[:]), SourceID: sourceID}
	if replay, replayErr := service.catalog.MediaOperationReceipt(ctx, operation); replayErr == nil {
		stored, decodeErr := canonical.Decode[store.MediaPublicationReceipt]([]byte(replay))
		return mediaReceiptFromStore(stored), decodeErr
	} else if !errors.Is(replayErr, store.ErrNotFound) {
		return MediaReceipt{}, replayErr
	}
	current, err := service.catalog.MediaSource(ctx, service.principal, sourceID)
	if err != nil || current.ContentVersionID == "" {
		return MediaReceipt{}, store.ErrNotFound
	}
	version, err := service.catalog.ContentVersionByID(ctx, current.ContentVersionID)
	if err != nil {
		return MediaReceipt{}, err
	}
	profile, err := service.mediaProcessingProfile(processingRequest.Profile)
	if err != nil {
		return MediaReceipt{}, err
	}
	source := mediaSourceBinding{sourceID: current.SourceID, sourceVersionID: current.SourceVersionID}
	binding, err := service.resolveMediaInputBinding(ctx, processingRequest.Profile,
		version.BlobHash, source, processingRequest.SuppliedInputID)
	if err != nil {
		return MediaReceipt{}, err
	}
	processingRequest.SuppliedInputID = binding
	selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: processingRequest.Profile}
	plan, err := service.Plan(ctx, selector)
	if err != nil {
		return MediaReceipt{}, err
	}
	authorization := service.renditionConsentRequest(profile)
	authorized, err := service.catalog.AuthorizeProviderOperation(ctx, authorization)
	if err != nil {
		return MediaReceipt{}, processingConsentBoundaryError(err)
	}
	authorization.PriorAuthorization = &authorized
	receipt := store.MediaPublicationReceipt{VaultUID: service.catalog.VaultID(), SourceID: sourceID,
		SourceVersionID: current.SourceVersionID, ContentVersionID: current.ContentVersionID,
		OccurrenceID: current.OccurrenceID, OperationID: operationID, OperationState: "queued",
		CoverageState: "pending", ProcessingNodeID: version.NodeID,
		ProcessingProfile: processingRequest.Profile, SuppliedInputID: processingRequest.SuppliedInputID,
		ProcessingPrincipal: service.principal, ProcessingScope: service.scope,
		ProcessingProfileFingerprint: profile.record.Fingerprint,
		ProcessingAuthorization:      authorization}
	var stored store.MediaPublicationReceipt
	err = service.mediaMutation(ctx, func() error {
		var queueErr error
		stored, queueErr = service.catalog.QueueMediaRetry(ctx, operation, receipt)
		return queueErr
	})
	if err != nil || stored.JobID != "" {
		return mediaReceiptFromStore(stored), err
	}
	job, err := service.EnqueueAuthorized(ctx, selector, source, plan.Fingerprint,
		authorization, processingRequest.SuppliedInputID)
	if err != nil {
		return MediaReceipt{}, errors.Join(err, service.failMediaProcessing(ctx, stored, err))
	}
	stored, err = service.recordMediaProcessingJob(ctx, stored, job.ID)
	return mediaReceiptFromStore(stored), err
}

func hashMediaArtifactID(values ...string) string {
	h := sha256.New()
	for _, value := range append([]string{"media-input/v1"}, values...) {
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(h.Sum(nil))
}
