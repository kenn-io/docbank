package plaintext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	suppliedTextProviderID     = "supplied-text.in-process-v1"
	suppliedTextProfileVersion = "docbank-supplied-text-profile/v1"
	suppliedTextProvider       = providerutil.Provider("supplied-text")
)

// SuppliedTextSource resolves supplied text for a sealed source digest. Each provider
// instance must hold the source selected for its package occurrence. A missing
// value is represented by the zero SuppliedText value.
type SuppliedTextSource interface {
	SuppliedText(ctx context.Context, sealedSourceSHA256 string) (document.SuppliedText, error)
}

// SuppliedTextProfile fixes one occurrence binding and one evidence policy.
type SuppliedTextProfile struct {
	Source           SuppliedTextSource
	SourceBinding    string
	MaxDocumentChars int
}

type SuppliedTextProvider struct {
	descriptor     document.RenditionDescriptor
	source         SuppliedTextSource
	sourceBinding  string
	evidencePolicy document.EvidencePolicy
}

type suppliedTextProfileIdentity struct {
	SourceBinding  string                          `json:"source_binding"`
	EvidencePolicy document.EvidencePolicyIdentity `json:"evidence_policy"`
}

// SuppliedTextSourceBinding binds one package occurrence, sealed native digest, exact
// supplied-text digest, declared encoding, and policy digest into one identity.
func SuppliedTextSourceBinding(packageID, occurrenceID, sourceSHA256, textSHA256, encoding, policySHA256 string) (string, error) {
	if packageID == "" || occurrenceID == "" || encoding == "" ||
		!canonical.IsSHA256Hex(sourceSHA256) || !canonical.IsSHA256Hex(textSHA256) || !canonical.IsSHA256Hex(policySHA256) {
		return "", errors.New("invalid supplied-text binding")
	}
	raw, err := canonical.Marshal([]string{"supplied-text-binding/v1", packageID, occurrenceID, sourceSHA256, textSHA256, encoding, policySHA256})
	if err != nil {
		return "", fmt.Errorf("encode supplied-text binding: %w", err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// NewSuppliedTextProvider creates an immutable local provider profile for one selected occurrence.
func NewSuppliedTextProvider(profile SuppliedTextProfile) (*SuppliedTextProvider, error) {
	if providerutil.IsNil(profile.Source) {
		return nil, errors.New("supplied text: source is required")
	}
	if !canonical.IsSHA256Hex(profile.SourceBinding) {
		return nil, errors.New("supplied text: source binding must be a lowercase SHA-256")
	}
	policy, err := document.NewEvidencePolicy(profile.MaxDocumentChars)
	if err != nil {
		return nil, fmt.Errorf("supplied text: evidence policy: %w", err)
	}
	identity, err := canonical.Marshal(suppliedTextProfileIdentity{SourceBinding: profile.SourceBinding, EvidencePolicy: policy.Identity()})
	if err != nil {
		return nil, fmt.Errorf("supplied text: encode profile: %w", err)
	}
	policyInput := append([]byte(suppliedTextProfileVersion+"\x00"), identity...)
	policyDigest := sha256.Sum256(policyInput)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID:                suppliedTextProviderID,
		ContractVersion:   document.RenditionProviderContractVersion,
		PolicyFingerprint: hex.EncodeToString(policyDigest[:]),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "image", MediaType: "image/tiff", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "structured", MediaType: "application/json", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	if err != nil {
		return nil, fmt.Errorf("supplied text: construct descriptor: %w", err)
	}
	return &SuppliedTextProvider{descriptor: providerutil.CloneDescriptor(descriptor), source: profile.Source, sourceBinding: profile.SourceBinding, evidencePolicy: policy}, nil
}

// Descriptor returns this provider's immutable execution identity.
func (p *SuppliedTextProvider) Descriptor() document.RenditionDescriptor {
	if p == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(p.descriptor)
}

// Render resolves only caller-selected text; it never reads or extracts the native.
func (p *SuppliedTextProvider) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	if p == nil {
		return document.RenditionResult{}, errors.New("supplied text: provider is required")
	}
	metadata := upload.Metadata()
	if !providerutil.AllowsArtifact(authorization, document.EvidenceArtifactStructured) {
		return document.RenditionResult{}, suppliedTextProvider.Classified(document.RenditionErrorPolicyRejected, "authorization does not allow retaining supplied text", nil)
	}
	if metadata.InputBinding != "" && metadata.InputBinding != p.sourceBinding {
		return document.RenditionResult{}, suppliedTextProvider.Classified(document.RenditionErrorPolicyRejected, "selected supplied-text binding differs from provider profile", nil)
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, suppliedTextProvider.Canceled(err)
	}
	startedAt := time.Now().UTC()
	text, err := p.source.SuppliedText(ctx, metadata.SHA256)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return document.RenditionResult{}, suppliedTextProvider.Canceled(contextErr)
		}
		if providerErr, ok := errors.AsType[*document.RenditionProviderError](err); ok {
			return document.RenditionResult{}, providerErr
		}
		return document.RenditionResult{}, suppliedTextProvider.Classified(document.RenditionErrorTransient, "supplied text could not be resolved", err)
	}
	if strings.TrimSpace(text.Text) == "" {
		return document.RenditionResult{}, suppliedTextProvider.Classified(document.RenditionErrorUnsupportedInput, "no supplied text for the sealed source digest", nil)
	}
	sourceEvidence, artifact, err := document.BuildSuppliedTextSourceEvidenceV1(text, p.evidencePolicy)
	if err != nil {
		return document.RenditionResult{}, suppliedTextProvider.Classified(document.RenditionErrorMalformedEvidence, "supplied text is invalid", err)
	}
	sourceEvidence.Family = authorization.MediaFamily
	receipt, err := providerutil.NewReceipt(suppliedTextProvider, providerutil.Receipt{
		Descriptor: p.descriptor, Authorization: authorization, SourceSHA256: metadata.SHA256,
		OperationID: "supplied-text-" + authorization.RenditionRequestFingerprint,
		StartedAt:   startedAt, CompletedAt: time.Now().UTC(), Warnings: []string{"degraded_provenance"},
		Usage: document.RenditionUsage{Requests: 1, InputBytes: metadata.ByteLength, OutputBytes: int64(len(artifact.Payload)), Units: 1},
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return document.RenditionResult{Evidence: sourceEvidence, Artifacts: []document.RenditionArtifact{artifact}, Receipt: receipt}, nil
}

var _ document.RenditionProvider = (*SuppliedTextProvider)(nil)
