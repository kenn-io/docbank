// Package suppliedtranscript turns caller-supplied transcript text into audio evidence.
package suppliedtranscript

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
)

const (
	providerID     = "supplied-transcript.in-process-v1"
	profileVersion = "docbank-supplied-transcript-profile/v1"
	timestampForm  = "2006-01-02T15:04:05.000000000Z"
)

// Source resolves caller-supplied transcript text by the sealed audio digest.
// A missing transcript returns a zero SuppliedTranscript and nil error. A
// nonzero Provider with nonempty Text is usable. An empty Provider or empty
// Text is malformed and is rejected by the shared transcript helper.
type Source interface {
	Transcript(ctx context.Context, digest string) (document.SuppliedTranscript, error)
}

// Profile fixes the transcript source and the processing profile whose
// evidence limits the provider uses.
type Profile struct {
	Source            Source
	ProcessingProfile document.ProcessingProfileV1
}

// Provider renders supplied transcript text for an authorized audio upload.
type Provider struct {
	descriptor     document.RenditionDescriptor
	source         Source
	evidencePolicy document.EvidencePolicy
}

// New constructs one immutable local supplied-transcript provider.
func New(profile Profile) (*Provider, error) {
	if profile.Source == nil {
		return nil, errors.New("supplied transcript: source is required")
	}
	evidencePolicy, err := document.NewEvidencePolicyForProcessingProfile(profile.ProcessingProfile)
	if err != nil {
		return nil, fmt.Errorf("supplied transcript: processing profile: %w", err)
	}
	policyBytes, err := json.Marshal(evidencePolicy.Identity())
	if err != nil {
		return nil, fmt.Errorf("supplied transcript: encode evidence policy: %w", err)
	}
	policyDigest := sha256.Sum256(append([]byte(profileVersion+"\x00"), policyBytes...))
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID:                providerID,
		ContractVersion:   document.RenditionProviderContractVersion,
		PolicyFingerprint: hex.EncodeToString(policyDigest[:]),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "audio", MediaType: "audio/mpeg", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
	})
	if err != nil {
		return nil, fmt.Errorf("supplied transcript: construct descriptor: %w", err)
	}
	return &Provider{descriptor: cloneDescriptor(descriptor), source: profile.Source,
		evidencePolicy: evidencePolicy}, nil
}

// Descriptor returns the immutable provider identity fixed by the profile.
func (provider *Provider) Descriptor() document.RenditionDescriptor {
	if provider == nil {
		return document.RenditionDescriptor{}
	}
	return cloneDescriptor(provider.descriptor)
}

// EvidencePolicy returns the policy derived from the provider's declared
// processing profile.
func (provider *Provider) EvidencePolicy() document.EvidencePolicy {
	if provider == nil {
		return document.EvidencePolicy{}
	}
	return provider.evidencePolicy
}

// Render resolves the transcript by the authorized sealed audio digest. The
// rendering boundary owns reading and verifying the upload bytes.
func (provider *Provider) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if provider == nil {
		return document.RenditionResult{}, errors.New("supplied transcript: provider is required")
	}
	if _, err := document.ValidateRenditionProviderRequest(provider, upload, authorization); err != nil {
		return document.RenditionResult{}, err
	}
	metadata := upload.Metadata()
	startedAt := time.Now().UTC()
	transcript, err := provider.source.Transcript(ctx, metadata.SHA256)
	if err != nil {
		return document.RenditionResult{}, err
	}
	if transcript.Provider == "" && transcript.Text == "" {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorUnsupportedInput,
			"supplied transcript is unavailable", nil)
	}
	sourceEvidence, artifact, err := document.BuildTranscriptSourceEvidenceV1(transcript, provider.evidencePolicy)
	if err != nil {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorMalformedEvidence,
			"supplied transcript is invalid", err)
	}
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, fmt.Errorf("supplied transcript: fingerprint authorization: %w", err)
	}
	completedAt := time.Now().UTC()
	return document.RenditionResult{
		Evidence:  sourceEvidence,
		Artifacts: []document.RenditionArtifact{artifact},
		Receipt: document.RenditionReceipt{
			ProviderID: provider.descriptor.ID, DescriptorFingerprint: provider.descriptor.Fingerprint,
			PolicyFingerprint:           authorization.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint,
			SourceSHA256:                metadata.SHA256,
			OperationID:                 "supplied-transcript-" + authorization.RenditionRequestFingerprint[:24],
			StartedAt:                   startedAt.Format(timestampForm),
			CompletedAt:                 completedAt.Format(timestampForm),
			Warnings:                    []string{"degraded_provenance"},
			Usage: document.RenditionUsage{
				Requests: 1, InputBytes: metadata.ByteLength, OutputBytes: int64(len(artifact.Payload)), Units: 1,
			},
		},
	}, nil
}

func classifiedError(code document.RenditionErrorCode, message string, cause error) error {
	providerError, err := document.NewRenditionProviderError(code, message, 0, cause)
	if err != nil {
		return fmt.Errorf("supplied transcript: classify provider error: %w", err)
	}
	return providerError
}

func cloneDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = slices.Clone(value.SupportedFormats)
	value.ArtifactRoles = slices.Clone(value.ArtifactRoles)
	return value
}

var _ document.RenditionProvider = (*Provider)(nil)
