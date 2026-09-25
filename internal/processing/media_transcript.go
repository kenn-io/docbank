package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mediatranscript"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

const (
	mediaTranscriptEvidenceReady       = "ready"
	mediaTranscriptEvidencePending     = "pending"
	mediaTranscriptEvidenceUnavailable = "unavailable"
	mediaTranscriptEvidenceStale       = "stale"
	mediaTranscriptMaxArtifactBytes    = int64(64 << 20)
	mediaTranscriptMaxResponseBytes    = int64(64 << 20)
)

var (
	ErrMediaTranscriptInvalid     = errors.New("media transcript request is invalid")
	ErrMediaTranscriptStale       = errors.New("media transcript authority is stale")
	ErrMediaTranscriptUnavailable = errors.New("retained media transcript evidence is unavailable")
	ErrMediaTranscriptCorrupt     = errors.New("retained media transcript evidence is corrupt")
	ErrMediaTranscriptOversize    = errors.New("media transcript response is too large")
)

// MediaTranscriptRequest names one immutable media tuple. The expected
// content version is part of the request so a caller cannot accept a newer
// source revision by accident.
type MediaTranscriptRequest struct {
	SourceID, SourceVersionID, ContentVersionID string
}

type MediaTranscriptUnit struct {
	Text    string
	StartMS *int64
	EndMS   *int64
	Speaker string
}

type MediaTranscriptEvidence struct {
	Origin       string
	Provider     string
	Language     string
	Completeness string
	Truncated    bool // Reports build truncation; Units retain the matched transcript artifact's text.
	HasOmissions bool
	Units        []MediaTranscriptUnit
}

// MediaTranscript is the complete safe result of an exact retained-evidence
// read. Transcript is nil unless EvidenceState is ready.
type MediaTranscript struct {
	VaultUID         string
	SourceID         string
	SourceVersionID  string
	ContentVersionID string
	EvidenceState    string
	CoverageState    string
	OperationState   string
	Transcript       *MediaTranscriptEvidence
}

// MediaTranscript reads one exact source/version/content tuple. The read
// takes no mutation gate and reselects every mutable authority after blob I/O.
func (service *Service) MediaTranscript(
	ctx context.Context, request MediaTranscriptRequest,
) (MediaTranscript, error) {
	return service.mediaTranscript(ctx, request, nil)
}

func (service *Service) mediaTranscript(
	ctx context.Context, request MediaTranscriptRequest, blobs retrieval.MediaEvidenceBlobReader,
) (MediaTranscript, error) {
	result := MediaTranscript{VaultUID: serviceVaultID(service), SourceID: request.SourceID,
		SourceVersionID: request.SourceVersionID, ContentVersionID: request.ContentVersionID,
		EvidenceState: mediaTranscriptEvidenceUnavailable}
	if service == nil || service.catalog == nil || service.blobs == nil {
		return result, ErrMediaCapabilityUnavailable
	}
	if blobs == nil {
		blobs = service.blobs
	}
	if err := validateMediaTranscriptRequest(request); err != nil {
		return result, err
	}
	item, err := service.catalog.MediaSourceVersion(ctx, service.principal, request.SourceID, request.SourceVersionID)
	if err != nil {
		return result, err
	}
	result, err = service.mediaTranscriptState(ctx, request, item)
	if err != nil {
		return result, err
	}
	if item.ContentVersionID != request.ContentVersionID {
		result.EvidenceState = mediaTranscriptEvidenceStale
		return result, nil
	}
	if item.SourceSHA256 == "" {
		return result, nil
	}
	visibilityFence, err := service.catalog.MediaVisibilityFence(ctx, service.principal)
	if err != nil {
		return result, err
	}

	version, node, err := service.mediaTranscriptNode(ctx, item.ContentVersionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return result, nil
		}
		return result, err
	}
	if node.Kind != "file" || node.TrashedAt != nil {
		return result, nil
	}
	if !mediaTranscriptNodeReadable(version, node) {
		result.EvidenceState = mediaTranscriptEvidenceStale
		return result, nil
	}
	if item.SourceBytes <= 0 || version.BlobHash != item.SourceSHA256 || version.Size != item.SourceBytes {
		result.EvidenceState = mediaTranscriptEvidenceStale
		return result, nil
	}
	profileFingerprint, profileName := mediaTranscriptProfile(service, item)
	if profileFingerprint == "" {
		result.EvidenceState = mediaTranscriptNoEvidenceState(result, item.SourceVersionActive)
		return result, nil
	}
	view, err := service.catalog.ActiveRendition(ctx, request.ContentVersionID, profileFingerprint)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			result.EvidenceState = mediaTranscriptNoEvidenceState(result, item.SourceVersionActive)
			return result, nil
		}
		return result, err
	}
	if view.Attachment.Profile.Fingerprint != profileFingerprint {
		result.EvidenceState = mediaTranscriptEvidenceStale
		return result, nil
	}
	inputBinding, err := service.catalog.RenditionInputBinding(ctx, view.Build.ID)
	if err != nil {
		return result, err
	}
	if err := service.validateMediaTranscriptView(ctx, item, view, inputBinding, profileName); err != nil {
		if errors.Is(err, ErrMediaTranscriptStale) {
			result.EvidenceState = mediaTranscriptEvidenceStale
			return result, nil
		}
		return result, err
	}

	normalized, err := renditionArtifact(view.Build, "normalized_evidence", 2, mediaTranscriptMaxArtifactBytes)
	if err != nil {
		if errors.Is(err, retrieval.ErrMediaArtifactOversize) {
			return result, ErrMediaTranscriptOversize
		}
		return result, fmt.Errorf("normalized transcript evidence: %w", err)
	}
	normalizedBytes, err := retrieval.ReadExactArtifact(ctx, blobs, normalized.BlobHash,
		normalized.Size, mediaTranscriptMaxArtifactBytes)
	if err != nil {
		return result, classifyMediaTranscriptArtifactError(err)
	}
	evidence, err := canonical.DecodeWith(normalizedBytes,
		func(value document.NormalizedEvidenceV1) ([]byte, error) {
			encoded, _, marshalErr := document.MarshalNormalizedEvidenceV1(value)
			return encoded, marshalErr
		})
	evidenceChecksum := transcriptDigest(normalizedBytes)
	if err != nil || evidenceChecksum != normalized.BlobHash || evidenceChecksum != normalized.Checksum ||
		evidenceChecksum != view.Build.EvidenceChecksum {
		return result, fmt.Errorf("%w: normalized evidence checksum", ErrMediaTranscriptCorrupt)
	}

	transcriptRecord, ok := transcriptArtifact(view.Build, evidence)
	if !ok {
		result.EvidenceState = mediaTranscriptEvidenceUnavailable
		return result, nil
	}
	transcriptBytes, err := readMediaTranscriptArtifact(ctx, blobs, transcriptRecord, evidence)
	if err != nil {
		return result, classifyMediaTranscriptArtifactError(err)
	}
	if transcriptRecord.Checksum != transcriptRecord.BlobHash {
		return result, fmt.Errorf("%w: transcript artifact checksum", ErrMediaTranscriptCorrupt)
	}
	evidenceResult, err := decodeMediaTranscriptArtifact(transcriptBytes, evidence, view.Build.Truncated)
	if errors.Is(err, retrieval.ErrMediaArtifactOversize) {
		return result, ErrMediaTranscriptOversize
	}
	if err != nil {
		if errors.Is(err, errUnknownMediaTranscriptFormat) {
			result.EvidenceState = mediaTranscriptEvidenceUnavailable
			return result, nil
		}
		return result, err
	}
	if _, exceeded, err := canonical.BoundedSize(evidenceResult, mediaTranscriptMaxResponseBytes); err != nil {
		return result, fmt.Errorf("%w: serializing transcript: %w", ErrMediaTranscriptCorrupt, err)
	} else if exceeded {
		return result, ErrMediaTranscriptOversize
	}

	if err := service.recheckMediaTranscriptAuthority(ctx, request, item, version, view, inputBinding, visibilityFence); err != nil {
		if errors.Is(err, ErrMediaTranscriptStale) {
			result.EvidenceState = mediaTranscriptEvidenceStale
			result.Transcript = nil
			return result, nil
		}
		return result, err
	}
	result.EvidenceState = mediaTranscriptEvidenceReady
	result.Transcript = &evidenceResult
	return result, nil
}

func mediaTranscriptNodeReadable(version store.ContentVersion, node store.Node) bool {
	return node.Kind == "file" && node.TrashedAt == nil &&
		node.CurrentVersionID == version.ID
}

func serviceVaultID(service *Service) string {
	if service == nil || service.catalog == nil {
		return ""
	}
	return service.catalog.VaultID()
}

func validateMediaTranscriptRequest(request MediaTranscriptRequest) error {
	if request.SourceID == "" || request.SourceVersionID == "" || request.ContentVersionID == "" ||
		len(request.SourceID) > 256 || len(request.SourceVersionID) > 256 || len(request.ContentVersionID) > 256 {
		return ErrMediaTranscriptInvalid
	}
	parsed, err := uuid.Parse(request.ContentVersionID)
	if err != nil || parsed.Version() != uuid.Version(4) {
		return fmt.Errorf("%w: content_version_id", ErrMediaTranscriptInvalid)
	}
	return nil
}

func (service *Service) mediaTranscriptState(
	ctx context.Context, request MediaTranscriptRequest, item store.MediaSourceProjection,
) (MediaTranscript, error) {
	receipt, err := service.mediaSourceReceipt(ctx, item)
	if err != nil {
		return MediaTranscript{VaultUID: service.catalog.VaultID(), SourceID: request.SourceID,
			SourceVersionID: request.SourceVersionID, ContentVersionID: request.ContentVersionID,
			EvidenceState: mediaTranscriptEvidenceUnavailable}, err
	}
	return MediaTranscript{VaultUID: service.catalog.VaultID(), SourceID: request.SourceID,
		SourceVersionID: request.SourceVersionID, ContentVersionID: request.ContentVersionID,
		EvidenceState: mediaTranscriptEvidenceUnavailable, CoverageState: receipt.CoverageState,
		OperationState: receipt.OperationState}, nil
}

func mediaTranscriptAvailability(result MediaTranscript, hasEvidence bool) string {
	if hasEvidence {
		return mediaTranscriptEvidenceReady
	}
	if result.CoverageState == "transcribed" {
		return mediaTranscriptEvidenceUnavailable
	}
	switch result.OperationState {
	case "queued", "running":
		return mediaTranscriptEvidencePending
	default:
		return mediaTranscriptEvidenceUnavailable
	}
}

func mediaTranscriptNoEvidenceState(result MediaTranscript, sourceVersionActive bool) string {
	if !sourceVersionActive || result.CoverageState == "stale" {
		return mediaTranscriptEvidenceStale
	}
	return mediaTranscriptAvailability(result, false)
}

func mediaTranscriptProfile(service *Service, item store.MediaSourceProjection) (string, string) {
	receipt := item.CoverageReceipt
	if receipt == nil {
		receipt = item.ProcessingReceipt
	}
	if receipt == nil {
		return "", ""
	}
	if receipt.ProcessingProfileFingerprint != "" {
		return receipt.ProcessingProfileFingerprint, receipt.ProcessingProfile
	}
	if profile, ok := service.profiles[receipt.ProcessingProfile]; ok {
		return profile.record.Fingerprint, receipt.ProcessingProfile
	}
	return "", ""
}

func (service *Service) mediaTranscriptNode(
	ctx context.Context, contentVersionID string,
) (store.ContentVersion, store.Node, error) {
	version, err := service.catalog.ContentVersionByID(ctx, contentVersionID)
	if err != nil {
		return store.ContentVersion{}, store.Node{}, err
	}
	node, err := service.catalog.NodeByID(ctx, version.NodeID)
	return version, node, err
}

func renditionArtifact(
	build store.RenditionBuildRecord, role string, minSize, maxSize int64,
) (store.RenditionArtifactRecord, error) {
	for _, artifact := range build.Artifacts {
		if artifact.Role != role {
			continue
		}
		if artifact.Size < minSize {
			return store.RenditionArtifactRecord{}, fmt.Errorf("%w: artifact size", ErrMediaTranscriptCorrupt)
		}
		if artifact.Size > maxSize {
			return store.RenditionArtifactRecord{}, fmt.Errorf("%w: artifact size", retrieval.ErrMediaArtifactOversize)
		}
		if artifact.State != store.RenditionArtifactVerified || artifact.BlobHash == "" ||
			artifact.Checksum == "" || artifact.BlobHash != artifact.Checksum || artifact.Size <= 0 {
			return store.RenditionArtifactRecord{}, fmt.Errorf("%w: artifact authority", ErrMediaTranscriptCorrupt)
		}
		return artifact, nil
	}
	return store.RenditionArtifactRecord{}, fmt.Errorf("%w: artifact is absent", ErrMediaTranscriptCorrupt)
}

func transcriptArtifact(
	build store.RenditionBuildRecord, evidence document.NormalizedEvidenceV1,
) (store.RenditionArtifactRecord, bool) {
	var digest string
	for _, artifact := range evidence.Artifacts {
		if artifact.Role == document.EvidenceArtifactTranscript {
			digest = artifact.SHA256
			break
		}
	}
	if digest == "" {
		return store.RenditionArtifactRecord{}, false
	}
	for _, artifact := range build.Artifacts {
		if artifact.Role == string(document.EvidenceArtifactTranscript) &&
			artifact.State == store.RenditionArtifactVerified && artifact.BlobHash == digest &&
			artifact.Checksum == digest {
			return artifact, true
		}
	}
	return store.RenditionArtifactRecord{}, false
}

func readMediaTranscriptArtifact(
	ctx context.Context, blobs retrieval.MediaEvidenceBlobReader,
	artifact store.RenditionArtifactRecord, evidence document.NormalizedEvidenceV1,
) ([]byte, error) {
	return retrieval.ReadExactArtifact(ctx, blobs, artifact.BlobHash, artifact.Size,
		mediaTranscriptArtifactLimit(evidence))
}

func mediaTranscriptArtifactLimit(evidence document.NormalizedEvidenceV1) int64 {
	// Timed media evidence is segmented; legacy supplied text is degraded generic evidence.
	if (evidence.Family == "audio" || evidence.Family == "video") &&
		evidence.UnitKind == document.EvidenceUnitSegment {
		return mediatranscript.MaxArtifactBytes
	}
	return mediaTranscriptMaxArtifactBytes
}

func (service *Service) validateMediaTranscriptView(
	ctx context.Context, item store.MediaSourceProjection, view store.RenditionView, inputBinding, profileName string,
) error {
	_, suppliedProfile := suppliedInputKind(profileName)
	if view.Attachment.ContentVersionID != item.ContentVersionID || view.Head.ContentVersionID != item.ContentVersionID ||
		view.Build.SourceSHA256 != item.SourceSHA256 || inputBinding == "" && suppliedProfile {
		return ErrMediaTranscriptStale
	}
	if inputBinding == "" {
		return nil
	}
	visible, err := service.catalog.MediaInputBindingVisible(ctx, service.principal,
		item.SourceID, item.SourceVersionID, inputBinding)
	if err != nil {
		return err
	}
	if !visible {
		return ErrMediaTranscriptStale
	}
	for _, kind := range []string{store.MediaInputTranscript, store.MediaInputCaption} {
		_, inputErr := service.catalog.SuppliedTranscriptForSourceVersion(ctx, service.principal, kind,
			item.SourceID, item.SourceVersionID, inputBinding)
		if inputErr == nil {
			return nil
		}
		if !errors.Is(inputErr, store.ErrNotFound) {
			return inputErr
		}
	}
	return ErrMediaTranscriptStale
}

func (service *Service) recheckMediaTranscriptAuthority(
	ctx context.Context, request MediaTranscriptRequest, before store.MediaSourceProjection,
	version store.ContentVersion, view store.RenditionView, inputBinding string,
	visibilityFence int64,
) error {
	after, err := service.catalog.MediaSourceVersion(ctx, service.principal, request.SourceID, request.SourceVersionID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrMediaTranscriptStale
	}
	if err != nil {
		return err
	}
	if after.ContentVersionID != request.ContentVersionID || after.OccurrenceID != before.OccurrenceID ||
		after.SourceSHA256 != before.SourceSHA256 || after.SourceBytes != before.SourceBytes {
		return ErrMediaTranscriptStale
	}
	currentVersion, currentNode, err := service.mediaTranscriptNode(ctx, request.ContentVersionID)
	if err != nil {
		return ErrMediaTranscriptStale
	}
	if currentVersion.BlobHash != after.SourceSHA256 || currentVersion.Size != after.SourceBytes ||
		currentVersion.BlobHash != version.BlobHash || currentVersion.Size != version.Size ||
		currentNode.CurrentVersionID != version.ID || currentNode.TrashedAt != nil {
		return ErrMediaTranscriptStale
	}
	currentView, err := service.catalog.ActiveRendition(ctx, request.ContentVersionID, view.Attachment.Profile.Fingerprint)
	if err != nil || currentView.Attachment.ID != view.Attachment.ID || currentView.Build.ID != view.Build.ID {
		return ErrMediaTranscriptStale
	}
	currentBinding, err := service.catalog.RenditionInputBinding(ctx, currentView.Build.ID)
	if err != nil || currentBinding != inputBinding {
		return ErrMediaTranscriptStale
	}
	if inputBinding != "" {
		visible, visibleErr := service.catalog.MediaInputBindingVisible(ctx, service.principal,
			request.SourceID, request.SourceVersionID, inputBinding)
		if visibleErr != nil {
			return visibleErr
		}
		if !visible {
			return ErrMediaTranscriptStale
		}
	}
	afterFence, err := service.catalog.MediaVisibilityFence(ctx, service.principal)
	if err != nil {
		return err
	}
	if visibilityFence != afterFence {
		return ErrMediaTranscriptStale
	}
	return nil
}

var errUnknownMediaTranscriptFormat = errors.New("unknown media transcript format")

type legacyMediaTranscriptArtifact struct {
	ContractVersion string `json:"contract_version"`
	Provider        string `json:"provider"`
	Text            string `json:"text"`
}

type mediaTranscriptEnvelope struct {
	ContractVersion string `json:"contract_version"`
}

func decodeMediaTranscriptArtifact(
	raw []byte, evidence document.NormalizedEvidenceV1, truncated bool,
) (MediaTranscriptEvidence, error) {
	var envelope mediaTranscriptEnvelope
	if err := json.Unmarshal(raw, &envelope, json.RejectUnknownMembers(false)); err != nil {
		return MediaTranscriptEvidence{}, fmt.Errorf("%w: envelope: %w", ErrMediaTranscriptCorrupt, err)
	}
	switch envelope.ContractVersion {
	case "media-transcript/v1":
		if int64(len(raw)) > mediatranscript.MaxArtifactBytes {
			return MediaTranscriptEvidence{}, retrieval.ErrMediaArtifactOversize
		}
		artifact, err := mediatranscript.Unmarshal(raw)
		if err != nil {
			return MediaTranscriptEvidence{}, fmt.Errorf("%w: timed transcript: %w", ErrMediaTranscriptCorrupt, err)
		}
		units := make([]MediaTranscriptUnit, len(artifact.Segments))
		for i, segment := range artifact.Segments {
			start, end := segment.StartMS, segment.EndMS
			units[i] = MediaTranscriptUnit{Text: segment.Text, Speaker: segment.Speaker, StartMS: &start, EndMS: &end}
		}
		return MediaTranscriptEvidence{Origin: artifact.Origin, Provider: artifact.Provider,
			Language: artifact.Language, Completeness: string(evidence.Completeness), Truncated: truncated,
			HasOmissions: mediaEvidenceHasOmissions(evidence), Units: units}, nil
	case "supplied-transcript/v1":
		artifact, err := canonical.Decode[legacyMediaTranscriptArtifact](raw)
		if err != nil || artifact.ContractVersion != "supplied-transcript/v1" || artifact.Provider == "" ||
			!utf8.ValidString(artifact.Text) || strings.TrimSpace(artifact.Text) == "" {
			return MediaTranscriptEvidence{}, fmt.Errorf("%w: supplied transcript", ErrMediaTranscriptCorrupt)
		}
		return MediaTranscriptEvidence{Origin: "supplied", Provider: artifact.Provider,
			Completeness: string(evidence.Completeness), Truncated: truncated,
			HasOmissions: mediaEvidenceHasOmissions(evidence), Units: []MediaTranscriptUnit{{Text: artifact.Text}}}, nil
	default:
		return MediaTranscriptEvidence{}, errUnknownMediaTranscriptFormat
	}
}

func mediaEvidenceHasOmissions(evidence document.NormalizedEvidenceV1) bool {
	if len(evidence.Omissions) > 0 {
		return true
	}
	for _, unit := range evidence.Units {
		if len(unit.Omissions) > 0 {
			return true
		}
	}
	return false
}

func classifyMediaTranscriptArtifactError(err error) error {
	switch {
	case errors.Is(err, retrieval.ErrMediaArtifactOversize):
		return ErrMediaTranscriptOversize
	case errors.Is(err, retrieval.ErrMediaArtifactUnavailable):
		return fmt.Errorf("%w: %w", ErrMediaTranscriptUnavailable, err)
	case errors.Is(err, retrieval.ErrMediaArtifactCorrupt):
		return fmt.Errorf("%w: %w", ErrMediaTranscriptCorrupt, err)
	default:
		return err
	}
}

func transcriptDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
