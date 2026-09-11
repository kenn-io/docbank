// Package suppliedtranscript implements the bounded in-process provider for
// caller-held transcripts resolved by sealed audio digests.
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
	"go.kenn.io/docbank/internal/canonical"
)

const (
	providerID     = "supplied-transcript.in-process-v1"
	profileVersion = "docbank-supplied-transcript-profile/v1"
	provider       = providerutil.Provider("supplied-transcript")
)

// Source resolves one caller-held transcript by the sealed audio digest. A
// digest with no transcript returns the zero SuppliedTranscript and a nil
// error. That is an ordinary outcome, rather than a failure. Return a
// *document.RenditionProviderError to classify a failure; other errors are
// treated as transient.
type Source interface {
	Transcript(ctx context.Context, sealedAudioSHA256 string) (document.SuppliedTranscript, error)
}

// Profile fixes the source identity and evidence policy for one
// provider instance.
type Profile struct {
	Source           Source
	SourceBinding    string
	MaxDocumentChars int
}

// Provider renders caller-held transcripts as audio evidence.
type Provider struct {
	descriptor     document.RenditionDescriptor
	source         Source
	evidencePolicy document.EvidencePolicy
}

type profileIdentity struct {
	SourceBinding  string                          `json:"source_binding"`
	EvidencePolicy document.EvidencePolicyIdentity `json:"evidence_policy"`
}

// New constructs one immutable local provider profile.
func New(profile Profile) (*Provider, error) {
	if providerutil.IsNil(profile.Source) {
		return nil, errors.New("supplied transcript: source is required")
	}
	if !canonical.IsSHA256Hex(profile.SourceBinding) {
		return nil, errors.New("supplied transcript: source binding must be a lowercase SHA-256")
	}
	evidencePolicy, err := document.NewEvidencePolicy(profile.MaxDocumentChars)
	if err != nil {
		return nil, fmt.Errorf("supplied transcript: evidence policy: %w", err)
	}
	identity, err := canonical.Marshal(profileIdentity{
		SourceBinding:  profile.SourceBinding,
		EvidencePolicy: evidencePolicy.Identity(),
	})
	if err != nil {
		return nil, fmt.Errorf("supplied transcript: encode profile: %w", err)
	}
	policyInput := append([]byte(profileVersion+"\x00"), identity...)
	policyDigest := sha256.Sum256(policyInput)
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
	return &Provider{
		descriptor:     providerutil.CloneDescriptor(descriptor),
		source:         profile.Source,
		evidencePolicy: evidencePolicy,
	}, nil
}

// Descriptor returns the immutable provider identity fixed by the profile.
func (p *Provider) Descriptor() document.RenditionDescriptor {
	if p == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(p.descriptor)
}

// Render resolves one transcript by the sealed audio digest and returns its
// source evidence and retained provider artifact.
func (p *Provider) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if p == nil {
		return document.RenditionResult{}, errors.New("supplied transcript: provider is required")
	}
	metadata := upload.Metadata()
	if !providerutil.AllowsArtifact(authorization, document.EvidenceArtifactTranscript) {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"authorization does not allow retaining the provider transcript", nil)
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, provider.Canceled(err)
	}
	startedAt := time.Now().UTC()
	transcript, err := p.source.Transcript(ctx, metadata.SHA256)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return document.RenditionResult{}, provider.Canceled(contextErr)
		}
		if providerErr, ok := errors.AsType[*document.RenditionProviderError](err); ok {
			return document.RenditionResult{}, providerErr
		}
		return document.RenditionResult{}, provider.Classified(
			document.RenditionErrorTransient, "supplied transcript could not be resolved", err)
	}
	if transcript == (document.SuppliedTranscript{}) {
		return document.RenditionResult{}, provider.Classified(
			document.RenditionErrorUnsupportedInput, "no supplied transcript for the sealed audio digest", nil)
	}
	sourceEvidence, artifact, err := document.BuildTranscriptSourceEvidenceV1(transcript, p.evidencePolicy)
	if err != nil {
		return document.RenditionResult{}, provider.Classified(
			document.RenditionErrorMalformedEvidence, "supplied transcript is invalid", err)
	}
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{
		Descriptor: p.descriptor, Authorization: authorization, SourceSHA256: metadata.SHA256,
		OperationID: "supplied-transcript-" + authorization.RenditionRequestFingerprint,
		StartedAt:   startedAt, CompletedAt: time.Now().UTC(), Warnings: []string{"degraded_provenance"},
		Usage: document.RenditionUsage{
			Requests: 1, InputBytes: metadata.ByteLength, OutputBytes: int64(len(artifact.Payload)), Units: 1,
		},
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return document.RenditionResult{
		Evidence:  sourceEvidence,
		Artifacts: []document.RenditionArtifact{artifact},
		Receipt:   receipt,
	}, nil
}

var _ document.RenditionProvider = (*Provider)(nil)
