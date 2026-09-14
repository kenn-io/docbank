package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/formatqualification"
)

func TestSyntheticMarkdownPDFOutput(t *testing.T) {
	descriptor := syntheticMarkdownCoverageDescriptor(t)
	source := []byte("%PDF-1.7\nsynthetic coverage fixture\n")
	digest := sha256.Sum256(source)
	metadata := document.AuthorizedUploadMetadata{
		Filename: "synthetic.pdf", MediaFamily: "pdf", MediaType: "application/pdf",
		ByteLength: int64(len(source)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("c", 64),
		ProviderMetadataChecksum: strings.Repeat("d", 64),
		InputKind:                document.RenditionInputOriginalFile,
	}
	authorizedAt := time.Now().UTC().Add(-time.Minute)
	authorization := document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("e", 64),
		SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType, InputKind: metadata.InputKind,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{
			document.EvidenceArtifactMarkdown, document.EvidenceArtifactStructured,
		},
		MaxProviderMarkdownBytes: 1024, MaxArtifactBytes: 1024,
		MaxArtifacts: 1, MaxTotalResultBytes: 4096,
		AuthorizedAt: authorizedAt.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:    authorizedAt.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}
	upload := &coverageAuthorizedUpload{ReadCloser: io.NopCloser(strings.NewReader(string(source))), metadata: metadata}

	result, err := document.RenderRendition(t.Context(), coverageMarkdownProvider{descriptor: descriptor}, upload, authorization)
	require.NoError(t, err)
	assert.Equal(t, "synthetic coverage fixture\n", string(result.ProviderMarkdown))
	assert.Equal(t, 1, upload.closes)
	_, found := formatqualification.Lookup(formatqualification.Query{
		CatalogID: "pdf", Capability: formatqualification.CapabilityText,
		Evidence: "TestSyntheticMarkdownPDFOutput", DescriptorFingerprint: descriptor.Fingerprint,
		InputKind: formatqualification.InputOriginalFile,
	})
	assert.True(t, found, "executed provider descriptor %s is absent from the immutable qualification manifest", descriptor.Fingerprint)
}

type coverageMarkdownProvider struct {
	descriptor document.RenditionDescriptor
}

func (provider coverageMarkdownProvider) Descriptor() document.RenditionDescriptor {
	return provider.descriptor
}

func (provider coverageMarkdownProvider) Render(
	_ context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	data, err := io.ReadAll(upload)
	if err != nil {
		return document.RenditionResult{}, err
	}
	markdown := []byte("synthetic coverage fixture\n")
	artifact := []byte(`{"kind":"synthetic"}`)
	artifactDigest := sha256.Sum256(artifact)
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, err
	}
	startedAt := time.Now().UTC()
	return document.RenditionResult{
		Evidence: document.SourceEvidenceV1{
			ContractVersion: document.SourceEvidenceContractV1,
			Completeness:    document.EvidenceDegradedProvenance,
			Family:          "pdf",
			Artifacts: []document.SourceEvidenceArtifactV1{{
				ProviderID: "synthetic-structured", Pointer: "provider/structured.json",
				Role: document.EvidenceArtifactStructured, SHA256: hex.EncodeToString(artifactDigest[:]),
			}},
			UnitKind: document.EvidenceUnitGeneric,
			Omissions: []document.SourceEvidenceOmissionV1{{
				Kind: document.EvidenceOmissionField, Field: "natural_provenance",
				Reason: "synthetic fixture has generic provenance",
			}},
			Units: []document.SourceEvidenceUnitV1{{
				Order: 0, Text: string(data),
				Locator: document.SourceEvidenceLocatorV1{
					Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
				},
			}},
		},
		ProviderMarkdown: markdown,
		Artifacts: []document.RenditionArtifact{{
			Role: document.EvidenceArtifactStructured, MediaType: "application/json",
			Payload: artifact, SHA256: hex.EncodeToString(artifactDigest[:]),
		}},
		Receipt: document.RenditionReceipt{
			ProviderID: provider.descriptor.ID, DescriptorFingerprint: provider.descriptor.Fingerprint,
			PolicyFingerprint:           authorization.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint,
			SourceSHA256:                authorization.SourceSHA256, OperationID: "synthetic-coverage-operation",
			StartedAt:   startedAt.Format("2006-01-02T15:04:05.000000000Z"),
			CompletedAt: startedAt.Add(time.Millisecond).Format("2006-01-02T15:04:05.000000000Z"),
			Warnings:    []string{"degraded_provenance"},
			Usage: document.RenditionUsage{
				Requests: 1, InputBytes: int64(len(data)),
				OutputBytes: int64(len(markdown) + len(artifact)), Units: 1,
			},
		},
	}, nil
}

type coverageAuthorizedUpload struct {
	io.ReadCloser

	metadata document.AuthorizedUploadMetadata
	closes   int
}

func (upload *coverageAuthorizedUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload.metadata
}

func (upload *coverageAuthorizedUpload) Close() error {
	upload.closes++
	return upload.ReadCloser.Close()
}

func syntheticMarkdownCoverageDescriptor(t *testing.T) document.RenditionDescriptor {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "synthetic-markdown", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("b", 64),
		TrustBoundary:     document.RenditionTrustOperatorNetwork,
		ReturnsMarkdown:   true,
		ReturnsStructured: true,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "pdf", MediaType: "application/pdf",
			InputKind: document.RenditionInputOriginalFile,
		}},
		ArtifactRoles: []document.EvidenceArtifactRole{
			document.EvidenceArtifactMarkdown, document.EvidenceArtifactStructured,
		},
	})
	require.NoError(t, err)
	return descriptor
}
