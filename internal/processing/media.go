package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

// MediaTimestamp preserves a caller's civil/source timestamp claim without
// inventing an instant for omitted or invalid source evidence.
type MediaTimestamp struct {
	Normalized     string `json:"normalized"`
	Raw            string `json:"raw"`
	Precision      string `json:"precision"`
	Timezone       string `json:"timezone"`
	ZoneText       string `json:"zone_text,omitempty"`
	OffsetSeconds  *int   `json:"offset_seconds,omitempty"`
	FractionDigits int    `json:"fraction_digits"`
}

type MediaProcessingRequest struct {
	Profile         string `json:"profile"`
	SuppliedInputID string `json:"supplied_input_id,omitempty"`
}

type MediaOccurrenceInput struct {
	Ref, Revision, Filename, PersonRef, SpeakerLabel string
	Message                                          MediaTimestamp
}

type SuppliedMediaRequest struct {
	OperationID              string
	Content                  io.Reader
	Filename, MediaType      string
	SHA256                   string
	ByteLength               int64
	ExistingContentVersionID string
	Occurrence               MediaOccurrenceInput
	Processing               *MediaProcessingRequest
}

type RemoteRecordingRequest struct {
	OperationID, ReferenceURL, CanonicalURL, ProviderHint, CredentialBinding string
	Acquire                                                                  bool
	Occurrence                                                               MediaOccurrenceInput
	Processing                                                               *MediaProcessingRequest
}

type MediaReceipt struct {
	VaultUID, SourceID, SourceVersionID, ContentVersionID, OccurrenceID string
	Outcome, CoverageState, OperationState                              string
	JobID, OperationID                                                  string
	SuppliedInputID                                                     string
}

// SubmitRemoteRecording retains one sanitized reference and occurrence. The
// manual original import path can later bind bytes to that occurrence.
func (service *Service) SubmitRemoteRecording(
	ctx context.Context, request RemoteRecordingRequest,
) (MediaReceipt, error) {
	if service == nil {
		return MediaReceipt{}, ErrMediaCapabilityUnavailable
	}
	if request.OperationID == "" || request.ReferenceURL == "" || len(request.ReferenceURL) > 8192 {
		return MediaReceipt{}, ErrMediaPlanInvalid
	}
	referenceDigest := sha256.Sum256([]byte(request.ReferenceURL))
	referenceSHA := hex.EncodeToString(referenceDigest[:])
	canonicalSHA := hashMediaPrivateValue(request.CanonicalURL)
	// Replay identity records caller input, independent of the policy that
	// admits a new source or the origin configuration after a restart.
	identity, err := canonical.Marshal(struct {
		ReferenceSHA256, ProviderHint, CredentialBindingSHA256 string
		CanonicalURLSHA256                                     string `json:",omitempty"`
		Acquire                                                bool
		Occurrence                                             MediaOccurrenceInput
		Processing                                             *MediaProcessingRequest
	}{ReferenceSHA256: referenceSHA, ProviderHint: request.ProviderHint, Acquire: request.Acquire,
		CanonicalURLSHA256: canonicalSHA, CredentialBindingSHA256: hashMediaPrivateValue(request.CredentialBinding),
		Occurrence: request.Occurrence, Processing: request.Processing})
	if err != nil {
		return MediaReceipt{}, err
	}
	requestDigest := sha256.Sum256(identity)
	operation := store.MediaOperation{ID: request.OperationID, Principal: service.principal,
		Verb: "submit_remote_recording", RequestSHA256: hex.EncodeToString(requestDigest[:])}
	if replay, replayErr := service.catalog.MediaOperationReceipt(ctx, operation); replayErr == nil {
		stored, decodeErr := canonical.Decode[store.MediaPublicationReceipt]([]byte(replay))
		return mediaReceiptFromStore(stored), decodeErr
	} else if !errors.Is(replayErr, store.ErrNotFound) {
		return MediaReceipt{}, replayErr
	}
	if request.Processing != nil {
		return MediaReceipt{}, ErrMediaProcessingUnsupported
	}
	provider, originScope, sourceKey, outcome := "", "", referenceSHA, ""
	if request.CanonicalURL != "" {
		canonicalURL, origin, canonicalErr := canonicalRemoteRecordingReference(request.CanonicalURL)
		if canonicalErr != nil {
			return MediaReceipt{}, ErrMediaPlanInvalid
		}
		if err := validateRemoteRecordingHints(request); err != nil {
			return MediaReceipt{}, err
		}
		videoID, capCloud := capCloudRecording(canonicalURL)
		if request.Acquire && !capCloud {
			return MediaReceipt{}, ErrMediaCapabilityUnavailable
		}
		provider = "url"
		originScope = hashMediaPrivateValue(origin)
		sourceKey = hashMediaPrivateValue(canonicalURL)
		if capCloud {
			// Cap documents no download route for received links, so an
			// acquisition request keeps the manual import path.
			provider = "cap"
			originScope = hashMediaPrivateValue(capCloudOrigin)
			sourceKey = hashMediaPrivateValue(videoID)
		}
		outcome = "unsupported"
	} else {
		policy, ok := service.recognizeMediaOrigin(request.ReferenceURL, request.ProviderHint)
		if !ok || request.Acquire {
			return MediaReceipt{}, ErrMediaCapabilityUnavailable
		}
		provider, originScope = policy.Provider, policy.OriginID
	}
	sourceID, err := store.MediaSourceKey("remote_recording", service.catalog.VaultID(),
		provider, originScope, sourceKey)
	if err != nil {
		return MediaReceipt{}, err
	}
	operation.SourceID = sourceID
	message, err := canonical.Marshal(request.Occurrence.Message)
	if err != nil {
		return MediaReceipt{}, err
	}
	occurrenceID := mediaOccurrenceID(service.principal, request.Occurrence.Ref, request.Occurrence.Revision)
	var stored store.MediaPublicationReceipt
	err = service.mediaMutation(ctx, func() error {
		var retainErr error
		stored, retainErr = service.catalog.RetainMediaReference(ctx, store.MediaReferencePublicationRequest{
			Operation: operation,
			Provider:  provider, OriginScope: originScope, IdentitySHA256: sourceID, Outcome: outcome,
			Occurrence: store.MediaOccurrenceInput{ID: occurrenceID, SourceID: sourceID,
				Principal: service.principal, Ref: request.Occurrence.Ref, Revision: request.Occurrence.Revision,
				Filename: request.Occurrence.Filename, PersonRef: request.Occurrence.PersonRef,
				SpeakerLabel: request.Occurrence.SpeakerLabel, MessageJSON: string(message)},
		})
		return retainErr
	})
	return mediaReceiptFromStore(stored), err
}

func canonicalRemoteRecordingReference(raw string) (canonicalURL, origin string, err error) {
	if raw == "" || len(raw) > 8192 || !utf8.ValidString(raw) {
		return "", "", errors.New("invalid canonical media reference")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Opaque != "" || parsed.Host == "" {
		return "", "", errors.New("invalid canonical media reference")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" || parsed.Hostname() == "" {
		return "", "", errors.New("invalid canonical media reference")
	}
	port := parsed.Port()
	if strings.HasSuffix(parsed.Host, ":") || port != "" {
		if port == "" {
			return "", "", errors.New("invalid canonical media reference")
		}
		portNumber, portErr := strconv.Atoi(port)
		if portErr != nil || portNumber < 1 || portNumber > 65535 {
			return "", "", errors.New("invalid canonical media reference")
		}
		port = strconv.Itoa(portNumber)
	}
	hostname := parsed.Hostname()
	if !strings.Contains(hostname, ":") {
		hostname, err = idna.Lookup.ToASCII(hostname)
		if err != nil || hostname == "" {
			return "", "", errors.New("invalid canonical media reference")
		}
	}
	hostname = strings.ToLower(hostname)
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" && (scheme != "http" || port != "80") && (scheme != "https" || port != "443") {
		host = net.JoinHostPort(hostname, port)
	}
	parsed.Scheme, parsed.Host, parsed.Fragment, parsed.RawFragment = scheme, host, "", ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String(), scheme + "://" + host, nil
}

func validateRemoteRecordingHints(request RemoteRecordingRequest) error {
	for _, field := range []struct {
		name, value string
		maximum     int
	}{
		{"provider hint", request.ProviderHint, 128},
		{"credential binding", request.CredentialBinding, 256},
	} {
		if !utf8.ValidString(field.value) || len(field.value) > field.maximum {
			return fmt.Errorf("media %s must be bounded UTF-8", field.name)
		}
	}
	return nil
}

func hashMediaPrivateValue(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type suppliedOperationIdentity struct {
	SourceSHA256             string                  `json:"source_sha256"`
	SourceBytes              int64                   `json:"source_bytes"`
	MediaType                string                  `json:"media_type"`
	Filename                 string                  `json:"filename"`
	ExistingContentVersionID string                  `json:"existing_content_version_id,omitempty"`
	Occurrence               MediaOccurrenceInput    `json:"occurrence"`
	Processing               *MediaProcessingRequest `json:"processing,omitempty"`
}

// SubmitSuppliedMedia retains one bounded WAV or MP3 and atomically binds its
// immutable source and occurrence authority. Provider work is handled by the
// authorized enqueue path and is never implied by retention.
func (service *Service) SubmitSuppliedMedia(
	ctx context.Context, request SuppliedMediaRequest,
) (MediaReceipt, error) {
	if service == nil {
		return MediaReceipt{}, errors.New("media capability is unavailable")
	}
	if err := validateSuppliedMediaRequest(request); err != nil {
		return MediaReceipt{}, err
	}
	messageJSON, err := canonical.Marshal(request.Occurrence.Message)
	if err != nil {
		return MediaReceipt{}, fmt.Errorf("encoding media timestamp: %w", err)
	}
	claim := sha256.Sum256(messageJSON)
	requestIdentity, err := canonical.Marshal(suppliedOperationIdentity{
		SourceSHA256: request.SHA256, SourceBytes: request.ByteLength,
		MediaType: request.MediaType, Filename: request.Filename,
		ExistingContentVersionID: request.ExistingContentVersionID,
		Occurrence:               request.Occurrence, Processing: request.Processing,
	})
	if err != nil {
		return MediaReceipt{}, fmt.Errorf("encoding supplied media request: %w", err)
	}
	requestDigest := sha256.Sum256(requestIdentity)
	sourceID, err := store.MediaSourceKey("supplied_media", service.catalog.VaultID(), "", "", request.SHA256)
	if err != nil {
		return MediaReceipt{}, err
	}
	operation := store.MediaOperation{ID: request.OperationID, Principal: service.principal,
		Verb: "submit_supplied_media", RequestSHA256: hex.EncodeToString(requestDigest[:]), SourceID: sourceID}
	if replay, replayErr := service.catalog.MediaOperationReceipt(ctx, operation); replayErr == nil {
		stored, decodeErr := canonical.Decode[store.MediaPublicationReceipt]([]byte(replay))
		return mediaReceiptFromStore(stored), decodeErr
	} else if !errors.Is(replayErr, store.ErrNotFound) {
		return MediaReceipt{}, replayErr
	}

	var processingProfile configuredProfile
	var processingAuthorization store.ProviderOperationAuthorizationRequest
	if mediaProcessingRequested(request.Processing) {
		processingProfile, err = service.mediaProcessingProfile(request.Processing.Profile)
		if err != nil {
			return MediaReceipt{}, err
		}
		binding, bindingErr := service.resolveMediaInputBinding(ctx, request.Processing.Profile,
			request.SHA256, mediaSourceBinding{sourceID: sourceID}, request.Processing.SuppliedInputID)
		if bindingErr != nil {
			return MediaReceipt{}, bindingErr
		}
		request.Processing.SuppliedInputID = binding
		processingAuthorization = service.renditionConsentRequest(processingProfile)
		authorized, authorizeErr := service.catalog.AuthorizeProviderOperation(ctx, processingAuthorization)
		if authorizeErr != nil {
			return MediaReceipt{}, processingConsentBoundaryError(authorizeErr)
		}
		processingAuthorization.PriorAuthorization = &authorized
	}
	filename := request.Occurrence.Filename
	if filename == "" {
		filename = request.Filename
	}
	processingPrincipal, processingScope, processingFingerprint := "", "", ""
	if mediaProcessingRequested(request.Processing) {
		processingPrincipal, processingScope = service.principal, service.scope
		processingFingerprint = processingProfile.record.Fingerprint
	}
	publication := store.MediaPublicationRequest{
		Operation: operation,
		SourceID:  sourceID, CaptureJSON: string(messageJSON),
		ClaimSHA256:         hex.EncodeToString(claim[:]),
		ProcessingProfile:   selectedProcessingProfile(request.Processing),
		SuppliedInputID:     selectedProcessingInputID(request.Processing),
		ProcessingPrincipal: processingPrincipal, ProcessingScope: processingScope,
		ProcessingProfileFingerprint: processingFingerprint,
		ProcessingAuthorization:      processingAuthorization,
		Occurrence: store.MediaOccurrenceInput{SourceID: sourceID, Principal: service.principal,
			Ref: request.Occurrence.Ref, Revision: request.Occurrence.Revision, Filename: filename,
			PersonRef: request.Occurrence.PersonRef, SpeakerLabel: request.Occurrence.SpeakerLabel,
			MessageJSON: string(messageJSON)},
	}
	var stored store.MediaPublicationReceipt
	if request.ExistingContentVersionID != "" {
		version, err := service.catalog.ContentVersionByID(ctx, request.ExistingContentVersionID)
		if err != nil {
			return MediaReceipt{}, err
		}
		if version.BlobHash != request.SHA256 || version.Size != request.ByteLength ||
			!mediaTypeMatches(request.MediaType, version.MimeType) {
			return MediaReceipt{}, store.ErrMediaSourceConflict
		}
		reader, _, readErr := service.blobs.OpenStreamContext(ctx, version.BlobHash)
		if readErr != nil {
			return MediaReceipt{}, readErr
		}
		request.Content = reader
		stored, err = service.stageAndRetainMedia(ctx, request, publication, &version)
		if err = errors.Join(err, reader.Close()); err != nil {
			return MediaReceipt{}, err
		}
	} else {
		var reuse *store.ContentVersion
		retained, retainedErr := service.catalog.SuppliedMediaContentVersion(
			ctx, sourceID, request.SHA256, request.ByteLength)
		if retainedErr == nil {
			reuse = &retained
		} else if !errors.Is(retainedErr, store.ErrNotFound) {
			return MediaReceipt{}, retainedErr
		}
		stored, err = service.stageAndRetainMedia(ctx, request, publication, reuse)
		if err != nil {
			return MediaReceipt{}, err
		}
	}
	if mediaProcessingRequested(request.Processing) && stored.JobID == "" && stored.OperationState == "queued" {
		selector := Selector{NodeID: stored.ProcessingNodeID, ContentVersionID: stored.ContentVersionID,
			Profile: request.Processing.Profile}
		source := mediaSourceBinding{sourceID: stored.SourceID, sourceVersionID: stored.SourceVersionID}
		plan, planErr := service.Plan(ctx, selector)
		if planErr != nil {
			return MediaReceipt{}, errors.Join(planErr, service.failMediaProcessing(ctx, stored, planErr))
		}
		job, enqueueErr := service.EnqueueAuthorized(ctx, selector, source, plan.Fingerprint,
			processingAuthorization, request.Processing.SuppliedInputID)
		if enqueueErr != nil {
			return MediaReceipt{}, errors.Join(enqueueErr, service.failMediaProcessing(ctx, stored, enqueueErr))
		}
		stored, err = service.recordMediaProcessingJob(ctx, stored, job.ID)
		if err != nil {
			return MediaReceipt{}, err
		}
	}
	return mediaReceiptFromStore(stored), nil
}

func mediaReceiptFromStore(stored store.MediaPublicationReceipt) MediaReceipt {
	result := MediaReceipt{VaultUID: stored.VaultUID, SourceID: stored.SourceID,
		SourceVersionID: stored.SourceVersionID, ContentVersionID: stored.ContentVersionID,
		OccurrenceID: stored.OccurrenceID, OperationID: stored.OperationID,
		Outcome: stored.Outcome, CoverageState: stored.CoverageState,
		OperationState: stored.OperationState, JobID: stored.JobID}
	result.SuppliedInputID = stored.SuppliedInputID
	return result
}

func selectedProcessingProfile(request *MediaProcessingRequest) string {
	if request == nil {
		return ""
	}
	return request.Profile
}

func selectedProcessingInputID(request *MediaProcessingRequest) string {
	if request == nil {
		return ""
	}
	return request.SuppliedInputID
}

// stageAndRetainMedia publishes core content and media authority together.
// Loose bytes written before a failed catalog commit remain untracked and can
// be reclaimed by the existing daemon GC, as with other interrupted ingests.
func (service *Service) stageAndRetainMedia(
	ctx context.Context, request SuppliedMediaRequest, publication store.MediaPublicationRequest,
	reuse *store.ContentVersion,
) (store.MediaPublicationReceipt, error) {
	staged, owned, err := service.mediaStagedContent(
		ctx, request.Content, request.ByteLength, service.mediaMaxBytes, request.SHA256)
	if err != nil {
		return store.MediaPublicationReceipt{}, err
	}
	if owned {
		defer func() { _ = staged.Close() }()
	}
	record, err := media.InspectCapability(staged, mediaInspectionPolicy(request, service.mediaMaxBytes))
	if err != nil {
		return store.MediaPublicationReceipt{}, err
	}
	if !record.Eligible || (record.Format != "wav" && record.Format != "mp3") {
		return store.MediaPublicationReceipt{}, fmt.Errorf("unqualified_codec: %s", record.Reason)
	}
	if reuse != nil {
		if reuse.BlobHash != request.SHA256 || reuse.Size != request.ByteLength ||
			!mediaTypeMatches(request.MediaType, reuse.MimeType) || !mediaTypeMatches(reuse.MimeType, record.MediaType) {
			return store.MediaPublicationReceipt{}, store.ErrMediaSourceConflict
		}
		publication.ContentVersion = *reuse
	} else {
		publication.ContentVersion = store.ContentVersion{BlobHash: request.SHA256,
			Size: request.ByteLength, MimeType: record.MediaType}
		publication.VirtualPath = path.Join("/media", request.SHA256[:2], request.SHA256+"."+record.Format)
		if err := staged.rewind(); err != nil {
			return store.MediaPublicationReceipt{}, err
		}
	}
	var stored store.MediaPublicationReceipt
	err = service.mediaMutation(ctx, func() error {
		return service.blobs.WithMutation(ctx, func() error {
			if reuse == nil {
				written, err := service.blobs.WriteDetailedContext(ctx, staged)
				if err != nil {
					return err
				}
				if written.Hash != request.SHA256 || written.Size != request.ByteLength {
					return errors.New("media staged identity changed during seal")
				}
				encoding, err := written.EncodingName()
				if err != nil {
					return err
				}
				publication.Physical = store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize,
					PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created}
			}
			var err error
			stored, err = service.catalog.RetainSuppliedMedia(ctx, publication)
			return err
		})
	})
	return stored, err
}

func mediaInspectionPolicy(request SuppliedMediaRequest, maxBytes int64) media.InspectionPolicy {
	return mediaInspectionPolicyForFile(request.Filename, request.MediaType, request.SHA256,
		request.ByteLength, maxBytes)
}

func mediaInspectionPolicyForFile(
	filename, mediaType, sha256 string, byteLength, maxBytes int64,
) media.InspectionPolicy {
	return media.InspectionPolicy{
		Filename: filename, DeclaredMediaType: mediaType,
		ExpectedBytes: byteLength, ExpectedSHA256: sha256,
		DescriptorFingerprint: sha256, ProfileFingerprint: sha256,
		DisclosureFingerprint: sha256, InputKind: document.RenditionInputOriginalFile,
		MaxSourceBytes: maxBytes, MaxExpandedBytes: maxBytes, MaxEntryBytes: maxBytes,
		MaxEntries: 100, MaxNestingDepth: 1, MaxTextLines: 1_000_000,
		MaxCharacters: maxBytes, MaxRecords: 1_000_000, MaxPages: 100_000,
		MaxSlides: 100_000, MaxSheets: 100_000, MaxCells: 10_000_000,
		MaxSpineItems: 100_000, MaxResources: 1_000_000,
		MaxDurationMS: 24 * 60 * 60 * 1000,
	}
}

func (service *Service) reserveMediaBytes(size int64) bool {
	service.mediaMu.Lock()
	defer service.mediaMu.Unlock()
	if size < 0 || size > service.mediaMaxBytes-service.mediaStagedBytes {
		return false
	}
	service.mediaStagedBytes += size
	return true
}

func (service *Service) releaseMediaBytes(size int64) {
	service.mediaMu.Lock()
	service.mediaStagedBytes -= size
	service.mediaMu.Unlock()
}

func validateSuppliedMediaRequest(request SuppliedMediaRequest) error {
	if request.ExistingContentVersionID == "" && request.Content == nil {
		return errors.New("supplied media content is required")
	}
	if request.ByteLength < 1 || !canonical.IsSHA256Hex(request.SHA256) {
		return errors.New("supplied media identity is invalid")
	}
	if err := validateMediaArtifactFile(request.Filename, request.MediaType); err != nil {
		return err
	}
	for _, field := range []struct {
		subject string
		value   string
		maximum int
	}{
		{"occurrence reference", request.Occurrence.Ref, 256},
		{"occurrence revision", request.Occurrence.Revision, 128},
		{"occurrence filename", request.Occurrence.Filename, 255},
		{"person reference", request.Occurrence.PersonRef, 256},
		{"speaker label", request.Occurrence.SpeakerLabel, 128},
	} {
		if !utf8.ValidString(field.value) || len(field.value) > field.maximum ||
			(field.value == "" && (field.subject == "filename" || field.subject == "media type" ||
				field.subject == "occurrence reference" || field.subject == "occurrence revision")) {
			return fmt.Errorf("media %s must be bounded UTF-8", field.subject)
		}
	}
	if err := validateMediaTimestamp(request.Occurrence.Message); err != nil {
		return err
	}
	return validateMediaProcessing(request.Processing)
}

func validateMediaArtifactFile(filename, mediaType string) error {
	for _, field := range []struct {
		subject string
		value   string
		maximum int
	}{
		{"filename", filename, 255}, {"media type", mediaType, 128},
	} {
		if !utf8.ValidString(field.value) || len(field.value) > field.maximum || field.value == "" {
			return fmt.Errorf("media %s must be bounded UTF-8", field.subject)
		}
	}
	if strings.ContainsAny(filename, "/\\\x00") || path.Base(filename) != filename {
		return errors.New("media filename must be a base name")
	}
	parsedMediaType, _, err := mime.ParseMediaType(mediaType)
	ext := strings.ToLower(path.Ext(filename))
	validIdentity := ext == ".wav" && (parsedMediaType == "audio/wav" || parsedMediaType == "audio/x-wav") ||
		ext == ".mp3" && parsedMediaType == "audio/mpeg"
	if err != nil || !validIdentity {
		return errors.New("supplied media requires a WAV or MP3 filename and media type")
	}
	return nil
}

func validateMediaTimestamp(value MediaTimestamp) error {
	for _, field := range []string{value.Normalized, value.Raw, value.Precision, value.Timezone, value.ZoneText} {
		if !utf8.ValidString(field) || len(field) > 4096 {
			return errors.New("media timestamp contains invalid text")
		}
	}
	if value.FractionDigits < 0 || value.FractionDigits > 9 {
		return errors.New("media timestamp fraction is invalid")
	}
	return nil
}

func mediaTypeMatches(declared, retained string) bool {
	declaredType, _, declaredErr := mime.ParseMediaType(declared)
	retainedType, _, retainedErr := mime.ParseMediaType(retained)
	if declaredErr != nil || retainedErr != nil {
		return false
	}
	if declaredType == "audio/x-wav" {
		declaredType = "audio/wav"
	}
	if retainedType == "audio/x-wav" {
		retainedType = "audio/wav"
	}
	return declaredType == retainedType
}
