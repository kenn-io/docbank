package upload_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/upload"
)

const proofTimestampForm = "2006-01-02T15:04:05.000000000Z"

func TestVerifiedUploadProofSurvivesRealCoreSealing(t *testing.T) {
	for _, disclose := range []bool{false, true} {
		t.Run(fmt.Sprintf("embedding disclose=%t", disclose), func(t *testing.T) {
			descriptor := proofEmbeddingDescriptor(t)
			source, facts := authorizeProofSource(t, descriptor.Fingerprint, disclose)
			provider := proofEmbeddingProvider{t: t, descriptor: descriptor, facts: facts, disclose: disclose}
			authorization := document.EmbeddingAuthorization{
				ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
				PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 1,
				MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20, DiscloseFilename: disclose,
			}
			result, err := document.ExecuteEmbedding(t.Context(), provider, []document.EmbeddingInput{{
				Key: "source", Role: document.EmbeddingRoleDocument,
				Kind: document.EmbeddingInputOriginalFile, Source: source,
			}}, authorization)
			require.NoError(t, err)
			assert.Equal(t, []document.EmbeddingVector{{Key: "source", Values: []float32{1, 0}}}, result.Vectors)
		})

		t.Run(fmt.Sprintf("rendition disclose=%t", disclose), func(t *testing.T) {
			descriptor := proofRenditionDescriptor(t)
			source, facts := authorizeProofSource(t, descriptor.Fingerprint, disclose)
			authorization := proofRenditionAuthorization(t, descriptor, source.Metadata(), disclose)
			provider := proofRenditionProvider{t: t, descriptor: descriptor, facts: facts, disclose: disclose}
			_, err := document.RenderRendition(t.Context(), provider, source, authorization)
			require.NoError(t, err)
		})
	}
}

type expectedProofFacts struct {
	bytes    []byte
	digest   string
	record   media.CapabilityRecord
	filename string
}

func authorizeProofSource(
	t *testing.T, descriptorFingerprint string, disclose bool,
) (document.AuthorizedUpload, expectedProofFacts) {
	t.Helper()
	data := []byte("synthetic proof text\n")
	digest := sha256.Sum256(data)
	digestHex := hex.EncodeToString(digest[:])
	disclosure := strings.Repeat("c", 64)
	if disclose {
		disclosure = strings.Repeat("d", 64)
	}
	policy := media.InspectionPolicy{
		Filename: "synthetic-proof.txt", DeclaredMediaType: "text/plain",
		ExpectedBytes: int64(len(data)), ExpectedSHA256: digestHex,
		DescriptorFingerprint: descriptorFingerprint, ProfileFingerprint: strings.Repeat("b", 64),
		DisclosureFingerprint: disclosure, InputKind: document.RenditionInputOriginalFile,
		MaxSourceBytes: 1 << 20, MaxExpandedBytes: 1 << 20, MaxEntryBytes: 1 << 20,
		MaxEntries: 100, MaxNestingDepth: 1, MaxTextLines: 1_000, MaxCharacters: 1 << 20,
		MaxPages: 100, MaxSlides: 100, MaxSheets: 100, MaxCells: 10_000,
		MaxSpineItems: 1_000, MaxResources: 10_000,
	}
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible)
	authorized, err := upload.Authorize(t.Context(), upload.Source{
		Reader: io.NopCloser(bytes.NewReader(data)), Directory: t.TempDir(),
	}, record, upload.UploadMetadata{Filename: "synthetic-proof.txt"})
	require.NoError(t, err)
	return authorized, expectedProofFacts{bytes: data, digest: digestHex, record: record, filename: policy.Filename}
}

func checkProofSource(t *testing.T, source document.AuthorizedUpload, want expectedProofFacts, disclose bool) {
	t.Helper()
	carrier, ok := source.(document.VerifiedUploadProofCarrier)
	require.True(t, ok)
	proof, present := carrier.VerifiedUploadProof()
	require.True(t, present)
	require.True(t, proof.Valid())
	facts := proof.Snapshot()
	assert.Equal(t, int64(len("synthetic proof text\n")), facts.SourceBytes)
	assert.Equal(t, want.digest, facts.SourceSHA256)
	assert.Equal(t, want.record.Checksum, facts.CapabilityRecordChecksum)
	assert.Equal(t, want.record.DescriptorFingerprint, facts.DescriptorFingerprint)
	assert.Equal(t, "text", facts.MediaFamily)
	assert.Equal(t, "text/plain", facts.MediaType)
	assert.Equal(t, "txt", facts.Format)
	if disclose {
		assert.Equal(t, want.filename, source.Metadata().Filename)
	} else {
		assert.Empty(t, source.Metadata().Filename)
	}
	assert.NotContains(t, fmt.Sprintf("%+v", proof), want.filename)
	_, marshalErr := jsonv1.Marshal(proof)
	require.Error(t, marshalErr)
	_, exposesRecord := source.(interface{ CapabilityRecord() media.CapabilityRecord })
	assert.False(t, exposesRecord)
	_, exposesReader := source.(interface{ Reader() io.Reader })
	assert.False(t, exposesReader)
	got, err := io.ReadAll(source)
	require.NoError(t, err)
	assert.Equal(t, want.bytes, got)
}

type proofEmbeddingProvider struct {
	t          *testing.T
	descriptor document.EmbeddingDescriptor
	facts      expectedProofFacts
	disclose   bool
}

func (provider proofEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}

func (provider proofEmbeddingProvider) Embed(
	_ context.Context, inputs []document.EmbeddingInput, _ document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	require.Len(provider.t, inputs, 1)
	checkProofSource(provider.t, inputs[0].Source, provider.facts, provider.disclose)
	return document.EmbeddingResult{Vectors: []document.EmbeddingVector{{
		Key: inputs[0].Key, Values: []float32{1, 0},
	}}}, nil
}

func proofEmbeddingDescriptor(t *testing.T) document.EmbeddingDescriptor {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileOpenAICompatible,
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-proof-embedder", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: strings.Repeat("1", 64), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-model", ModelRevision: "r1", Dimension: 2,
		Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationUnitLength,
		ScalarEncoding: "float32", DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile},
		CompatibilityID: contract.CompatibilityID, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	require.NoError(t, err)
	return descriptor
}

type proofRenditionProvider struct {
	t          *testing.T
	descriptor document.RenditionDescriptor
	facts      expectedProofFacts
	disclose   bool
}

func (provider proofRenditionProvider) Descriptor() document.RenditionDescriptor {
	return provider.descriptor
}

func (provider proofRenditionProvider) Render(
	_ context.Context, source document.AuthorizedUpload, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	checkProofSource(provider.t, source, provider.facts, provider.disclose)
	return proofRenditionResult(provider.t, provider.descriptor, authorization), nil
}

func proofRenditionDescriptor(t *testing.T) document.RenditionDescriptor {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "synthetic-proof-rendition", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("1", 64), TrustBoundary: document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile,
		}},
		ReturnsMarkdown: true, ReturnsStructured: true,
		ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	require.NoError(t, err)
	return descriptor
}

func proofRenditionAuthorization(
	t *testing.T, descriptor document.RenditionDescriptor, metadata document.AuthorizedUploadMetadata, disclose bool,
) document.RenditionAuthorization {
	t.Helper()
	authorizedAt := time.Now().UTC().Add(-time.Minute)
	return document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256: metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType, InputKind: metadata.InputKind,
		DiscloseFilename: disclose, AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
		MaxProviderMarkdownBytes: 1_024, MaxArtifactBytes: 1_024, MaxArtifacts: 1,
		MaxTotalResultBytes: 4_096, AuthorizedAt: authorizedAt.Format(proofTimestampForm),
		ExpiresAt: authorizedAt.Add(10 * time.Minute).Format(proofTimestampForm),
	}
}

func proofRenditionResult(
	t *testing.T, descriptor document.RenditionDescriptor, authorization document.RenditionAuthorization,
) document.RenditionResult {
	t.Helper()
	authorizationFingerprint, err := authorization.Fingerprint()
	require.NoError(t, err)
	authorizedAt, err := time.Parse(proofTimestampForm, authorization.AuthorizedAt)
	require.NoError(t, err)
	payload := []byte(`{"synthetic":"structured"}`)
	digest := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(digest[:])
	return document.RenditionResult{
		Evidence: document.SourceEvidenceV1{
			ContractVersion: document.SourceEvidenceContractV1,
			Completeness:    document.EvidenceDegradedProvenance, Family: "text",
			Artifacts: []document.SourceEvidenceArtifactV1{{
				ProviderID: "provider-artifact-1", Pointer: "provider/structured.json",
				Role: document.EvidenceArtifactStructured, SHA256: digestHex,
			}},
			UnitKind: document.EvidenceUnitGeneric,
			Omissions: []document.SourceEvidenceOmissionV1{{
				Kind: document.EvidenceOmissionField, Field: "natural_provenance",
				Reason: "synthetic provider returned generic evidence",
			}},
			Units: []document.SourceEvidenceUnitV1{{
				Order: 0, Text: "synthetic evidence", Locator: document.SourceEvidenceLocatorV1{
					Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
				},
			}},
		},
		ProviderMarkdown: []byte("synthetic evidence\n"),
		Artifacts: []document.RenditionArtifact{{
			Role: document.EvidenceArtifactStructured, MediaType: "application/json",
			Payload: payload, SHA256: digestHex,
		}},
		Receipt: document.RenditionReceipt{
			ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
			PolicyFingerprint:           authorization.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint, SourceSHA256: authorization.SourceSHA256,
			OperationID: "operation-synthetic-1",
			StartedAt:   authorizedAt.Add(time.Second).Format(proofTimestampForm),
			CompletedAt: authorizedAt.Add(2 * time.Second).Format(proofTimestampForm),
			Warnings:    []string{"degraded_provenance"},
			Usage: document.RenditionUsage{Requests: 1, InputBytes: authorization.SourceBytes,
				OutputBytes: int64(len(payload) + len("synthetic evidence\n")), Units: 1},
		},
	}
}
