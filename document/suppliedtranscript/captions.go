package suppliedtranscript

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/mediatranscript"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	captionProviderID     = "supplied-captions.in-process-v1"
	captionProfileVersion = "docbank-supplied-captions-profile/v1"
	captionProvider       = providerutil.Provider("supplied-captions")
)

// Caption is one exact caller-held SubRip file selected before provider
// admission. Provider and Language are caller claims.
type Caption struct {
	Provider string
	Language string
	SRT      []byte
}

// CaptionSource resolves the caption frozen into the execution identity.
// Return a *document.RenditionProviderError to classify a failure.
type CaptionSource interface {
	CaptionForBinding(ctx context.Context, sealedSourceSHA256, binding string) (Caption, error)
}

type CaptionProfile struct {
	Source           CaptionSource
	SourceBinding    string
	MaxDocumentChars int
}

// CaptionProvider renders supplied SubRip captions as timed evidence.
type CaptionProvider struct {
	descriptor     document.RenditionDescriptor
	source         CaptionSource
	evidencePolicy document.EvidencePolicy
}

type captionProfileIdentity struct {
	SourceBinding  string
	EvidencePolicy document.EvidencePolicyIdentity
	Parser         string
}

// NewCaptions constructs one immutable local caption provider profile.
func NewCaptions(profile CaptionProfile) (*CaptionProvider, error) {
	if providerutil.IsNil(profile.Source) {
		return nil, errors.New("supplied captions: source is required")
	}
	if !canonical.IsSHA256Hex(profile.SourceBinding) {
		return nil, errors.New("supplied captions: source binding must be a lowercase SHA-256")
	}
	evidencePolicy, err := document.NewEvidencePolicy(profile.MaxDocumentChars)
	if err != nil {
		return nil, fmt.Errorf("supplied captions: evidence policy: %w", err)
	}
	identity, err := canonical.Marshal(captionProfileIdentity{
		SourceBinding:  profile.SourceBinding,
		EvidencePolicy: evidencePolicy.Identity(),
		Parser:         mediatranscript.SubRipContractV1,
	})
	if err != nil {
		return nil, fmt.Errorf("supplied captions: encode profile: %w", err)
	}
	policyInput := append([]byte(captionProfileVersion+"\x00"), identity...)
	policyDigest := sha256.Sum256(policyInput)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID:                captionProviderID,
		ContractVersion:   document.RenditionProviderContractVersion,
		PolicyFingerprint: hex.EncodeToString(policyDigest[:]),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "audio", MediaType: "audio/mpeg", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "video", MediaType: "video/mp4", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
	})
	if err != nil {
		return nil, fmt.Errorf("supplied captions: construct descriptor: %w", err)
	}
	return &CaptionProvider{
		descriptor:     providerutil.CloneDescriptor(descriptor),
		source:         profile.Source,
		evidencePolicy: evidencePolicy,
	}, nil
}

// Descriptor returns the immutable provider identity fixed by the profile.
func (provider *CaptionProvider) Descriptor() document.RenditionDescriptor {
	if provider == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(provider.descriptor)
}

// Render parses the selected SubRip file and returns exact timed evidence.
func (provider *CaptionProvider) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if provider == nil {
		return document.RenditionResult{}, errors.New("supplied captions: provider is required")
	}
	if !providerutil.AllowsArtifact(authorization, document.EvidenceArtifactTranscript) {
		return document.RenditionResult{}, captionProvider.Classified(
			document.RenditionErrorPolicyRejected,
			"authorization does not allow retaining the caption transcript", nil)
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, captionProvider.Canceled(err)
	}
	if providerutil.IsNil(upload) {
		return document.RenditionResult{}, captionProvider.Classified(
			document.RenditionErrorPolicyRejected, "caption processing requires an authorized upload", nil)
	}
	metadata := upload.Metadata()
	if metadata.InputBinding == "" {
		return document.RenditionResult{}, captionProvider.Classified(
			document.RenditionErrorPolicyRejected, "caption processing requires a selected caption", nil)
	}
	durationMS, err := provider.captionDuration(upload, authorization)
	if err != nil {
		return document.RenditionResult{}, err
	}
	caption, err := provider.source.CaptionForBinding(ctx, metadata.SHA256, metadata.InputBinding)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return document.RenditionResult{}, captionProvider.Canceled(contextErr)
		}
		if providerErr, ok := errors.AsType[*document.RenditionProviderError](err); ok {
			return document.RenditionResult{}, providerErr
		}
		return document.RenditionResult{}, captionProvider.Classified(
			document.RenditionErrorTransient, "supplied caption could not be resolved", err)
	}
	if len(caption.SRT) == 0 {
		return document.RenditionResult{}, captionProvider.Classified(
			document.RenditionErrorUnsupportedInput, "no supplied caption for the selected binding", nil)
	}
	artifact, err := mediatranscript.ParseSubRip(caption.SRT, caption.Provider, caption.Language)
	if err != nil {
		return document.RenditionResult{}, captionProvider.Classified(
			document.RenditionErrorMalformedEvidence, "supplied caption is malformed", err)
	}
	for _, segment := range artifact.Segments {
		if segment.EndMS > durationMS {
			return document.RenditionResult{}, captionProvider.Classified(
				document.RenditionErrorMalformedEvidence,
				"supplied caption extends beyond the recording", nil)
		}
	}
	evidence, retained, err := mediatranscript.Build(artifact, authorization.MediaFamily, provider.evidencePolicy)
	if err != nil {
		return document.RenditionResult{}, captionProvider.Classified(
			document.RenditionErrorMalformedEvidence, "supplied caption evidence is invalid", err)
	}
	if len(retained.Payload) > authorization.MaxArtifactBytes {
		return document.RenditionResult{}, captionProvider.Classified(
			document.RenditionErrorMalformedEvidence, "supplied caption artifact exceeds its limit", nil)
	}
	startedAt := time.Now().UTC()
	receipt, err := providerutil.NewReceipt(captionProvider, providerutil.Receipt{
		Descriptor: provider.descriptor, Authorization: authorization, SourceSHA256: metadata.SHA256,
		OperationID: "supplied-captions-" + authorization.RenditionRequestFingerprint,
		StartedAt:   startedAt, CompletedAt: time.Now().UTC(),
		Usage: document.RenditionUsage{
			Requests: 1, InputBytes: metadata.ByteLength, OutputBytes: int64(len(retained.Payload)), Units: 1,
		},
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return document.RenditionResult{Evidence: evidence,
		Artifacts: []document.RenditionArtifact{retained}, Receipt: receipt}, nil
}

func (provider *CaptionProvider) captionDuration(
	upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
) (int64, error) {
	carrier, ok := upload.(interface {
		CapabilityProof() document.UploadCapability
	})
	if !ok {
		return 0, captionProvider.Classified(
			document.RenditionErrorPolicyRejected, "supplied captions require local media inspection", nil)
	}
	facts, local := carrier.CapabilityProof().Facts()
	metadata := upload.Metadata()
	if !local || facts.Checksum != authorization.CapabilityRecordChecksum ||
		facts.Checksum != metadata.CapabilityRecordChecksum || facts.SourceSHA256 != metadata.SHA256 ||
		facts.SourceBytes != metadata.ByteLength || facts.DescriptorFingerprint != provider.descriptor.Fingerprint ||
		facts.MediaFamily != metadata.MediaFamily || facts.MediaType != metadata.MediaType ||
		!captionFormatSupported(facts.MediaFamily, facts.MediaType) ||
		facts.MediaFamily != authorization.MediaFamily || facts.MediaType != authorization.MediaType ||
		facts.DurationMS <= 0 || facts.DurationMS > (24*time.Hour).Milliseconds() {
		return 0, captionProvider.Classified(
			document.RenditionErrorPolicyRejected, "caption media capability does not match the upload", nil)
	}
	return facts.DurationMS, nil
}

func captionFormatSupported(family, mediaType string) bool {
	switch {
	case family == "audio" && mediaType == "audio/mpeg":
		return true
	case family == "audio" && mediaType == "audio/wav":
		return true
	case family == "video" && mediaType == "video/mp4":
		return true
	default:
		return false
	}
}

var _ document.RenditionProvider = (*CaptionProvider)(nil)
