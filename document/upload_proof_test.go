package document

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/internal/uploadproof"
)

func TestCoreRejectsPresentInvalidOrMismatchedUploadProofBeforeProvider(t *testing.T) {
	metadata := validAuthorizedUploadMetadata()
	validProof := testVerifiedUploadProof(t, metadata, "descriptor-a")
	for _, testCase := range []struct {
		name  string
		proof VerifiedUploadProof
	}{
		{name: "invalid", proof: VerifiedUploadProof{}},
		{name: "source bytes mismatch", proof: mutateVerifiedUploadProof(t, validProof, func(facts *VerifiedUploadFacts) {
			facts.SourceBytes++
		})},
		{name: "source digest mismatch", proof: mutateVerifiedUploadProof(t, validProof, func(facts *VerifiedUploadFacts) {
			facts.SourceSHA256 = strings.Repeat("e", 64)
		})},
		{name: "capability checksum mismatch", proof: mutateVerifiedUploadProof(t, validProof, func(facts *VerifiedUploadFacts) {
			facts.CapabilityRecordChecksum = strings.Repeat("e", 64)
		})},
		{name: "media family mismatch", proof: mutateVerifiedUploadProof(t, validProof, func(facts *VerifiedUploadFacts) {
			facts.MediaFamily = "image"
		})},
		{name: "media type mismatch", proof: mutateVerifiedUploadProof(t, validProof, func(facts *VerifiedUploadFacts) {
			facts.MediaType = "image/png"
		})},
		{name: "input kind mismatch", proof: mutateVerifiedUploadProof(t, validProof, func(facts *VerifiedUploadFacts) {
			facts.InputKind = "derived_upload"
		})},
	} {
		t.Run(testCase.name+" rendition", func(t *testing.T) {
			descriptor := validRenditionDescriptor(t)
			authorization := validRenditionAuthorization(descriptor, metadata)
			renderCalls := 0
			provider := proofAwareRenditionProvider{descriptor: descriptor, render: func(AuthorizedUpload) {
				renderCalls++
			}}
			source := newProofSequenceUpload(metadata, testCase.proof)

			_, err := RenderRendition(t.Context(), provider, source, authorization)
			require.ErrorContains(t, err, "proof does not match metadata")
			assert.Zero(t, renderCalls)
		})

		t.Run(testCase.name+" embedding", func(t *testing.T) {
			descriptor := testProofEmbeddingDescriptor(t)
			embedCalls := 0
			provider := proofAwareEmbeddingProvider{descriptor: descriptor, embed: func(AuthorizedUpload) {
				embedCalls++
			}}
			source := newProofSequenceUpload(metadata, testCase.proof)

			_, err := ExecuteEmbedding(t.Context(), provider, []EmbeddingInput{{
				Key: "source", Role: EmbeddingRoleDocument, Kind: EmbeddingInputOriginalFile, Source: source,
			}}, testProofEmbeddingAuthorization(descriptor))
			require.ErrorContains(t, err, "proof does not match metadata")
			assert.Zero(t, embedCalls)
		})
	}
}

func mutateVerifiedUploadProof(
	t *testing.T, proof VerifiedUploadProof, mutate func(*VerifiedUploadFacts),
) VerifiedUploadProof {
	t.Helper()
	facts := proof.Snapshot()
	mutate(&facts)
	mutated, err := uploadproof.Issue(facts)
	require.NoError(t, err)
	return mutated
}

func TestCoreFreezesUploadProofBeforeProviderEntry(t *testing.T) {
	metadata := validAuthorizedUploadMetadata()
	first := testVerifiedUploadProof(t, metadata, "descriptor-a")
	second := testVerifiedUploadProof(t, metadata, "descriptor-b")

	t.Run("rendition", func(t *testing.T) {
		descriptor := validRenditionDescriptor(t)
		authorization := validRenditionAuthorization(descriptor, metadata)
		source := newProofSequenceUpload(metadata, first, second)
		provider := proofAwareRenditionProvider{descriptor: descriptor, render: func(upload AuthorizedUpload) {
			carrier, ok := upload.(VerifiedUploadProofCarrier)
			require.True(t, ok)
			proof, present := carrier.VerifiedUploadProof()
			require.True(t, present)
			assert.Equal(t, first.Snapshot(), proof.Snapshot())
			_, err := io.ReadAll(upload)
			require.NoError(t, err)
		}}

		_, err := RenderRendition(t.Context(), provider, source, authorization)
		require.NoError(t, err)
		assert.Equal(t, 1, source.proofCalls)
	})

	t.Run("embedding", func(t *testing.T) {
		descriptor := testProofEmbeddingDescriptor(t)
		source := newProofSequenceUpload(metadata, first, second)
		provider := proofAwareEmbeddingProvider{descriptor: descriptor, embed: func(upload AuthorizedUpload) {
			carrier, ok := upload.(VerifiedUploadProofCarrier)
			require.True(t, ok)
			proof, present := carrier.VerifiedUploadProof()
			require.True(t, present)
			assert.Equal(t, first.Snapshot(), proof.Snapshot())
			_, err := io.ReadAll(upload)
			require.NoError(t, err)
		}}

		_, err := ExecuteEmbedding(t.Context(), provider, []EmbeddingInput{{
			Key: "source", Role: EmbeddingRoleDocument, Kind: EmbeddingInputOriginalFile, Source: source,
		}}, testProofEmbeddingAuthorization(descriptor))
		require.NoError(t, err)
		assert.Equal(t, 1, source.proofCalls)
	})
}

type proofSequenceUpload struct {
	*syntheticAuthorizedUpload

	proofs     []VerifiedUploadProof
	proofCalls int
}

func newProofSequenceUpload(metadata AuthorizedUploadMetadata, proofs ...VerifiedUploadProof) *proofSequenceUpload {
	return &proofSequenceUpload{
		syntheticAuthorizedUpload: &syntheticAuthorizedUpload{
			ReadCloser: io.NopCloser(bytes.NewReader([]byte("synthetic exact source"))), metadata: metadata,
		},
		proofs: proofs,
	}
}

func (upload *proofSequenceUpload) VerifiedUploadProof() (VerifiedUploadProof, bool) {
	proof := upload.proofs[min(upload.proofCalls, len(upload.proofs)-1)]
	upload.proofCalls++
	return proof, true
}

func testVerifiedUploadProof(
	t *testing.T, metadata AuthorizedUploadMetadata, descriptorFingerprint string,
) VerifiedUploadProof {
	t.Helper()
	proof, err := uploadproof.Issue(uploadproof.Facts{
		SourceBytes: metadata.ByteLength, SourceSHA256: metadata.SHA256,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		DescriptorFingerprint:    strings.Repeat(descriptorFingerprint[len(descriptorFingerprint)-1:], 64),
		ProfileFingerprint:       strings.Repeat("c", 64), DisclosureFingerprint: strings.Repeat("d", 64),
		InputKind: string(metadata.InputKind), MediaFamily: metadata.MediaFamily,
		MediaType: metadata.MediaType, Format: "pdf", MaxSourceBytes: 1 << 20,
	})
	require.NoError(t, err)
	return proof
}

type proofAwareRenditionProvider struct {
	descriptor RenditionDescriptor
	render     func(AuthorizedUpload)
}

func (provider proofAwareRenditionProvider) Descriptor() RenditionDescriptor {
	return provider.descriptor
}

func (provider proofAwareRenditionProvider) Render(
	_ context.Context, upload AuthorizedUpload, authorization RenditionAuthorization,
) (RenditionResult, error) {
	provider.render(upload)
	return validRenditionResult(provider.descriptor, authorization), nil
}

type proofAwareEmbeddingProvider struct {
	descriptor EmbeddingDescriptor
	embed      func(AuthorizedUpload)
}

func (provider proofAwareEmbeddingProvider) Descriptor() EmbeddingDescriptor {
	return provider.descriptor
}

func (provider proofAwareEmbeddingProvider) Embed(
	_ context.Context, inputs []EmbeddingInput, _ EmbeddingAuthorization,
) (EmbeddingResult, error) {
	provider.embed(inputs[0].Source)
	return EmbeddingResult{Vectors: []EmbeddingVector{{Key: inputs[0].Key, Values: []float32{1, 0}}}}, nil
}

func testProofEmbeddingDescriptor(t *testing.T) EmbeddingDescriptor {
	t.Helper()
	contract, err := NewModelInputContract(ModelInputContractConfig{Profile: ModelInputProfileOpenAICompatible})
	require.NoError(t, err)
	descriptor, err := NewEmbeddingDescriptor(EmbeddingDescriptor{
		ID: "synthetic-proof-embedder", ContractVersion: EmbeddingProviderContractVersion,
		PolicyFingerprint: strings.Repeat("1", 64), TrustBoundary: EmbeddingTrustLocalProcess,
		Model: "synthetic-model", ModelRevision: "r1", Dimension: 2,
		Metric: VectorMetricCosine, Normalization: VectorNormalizationUnitLength,
		ScalarEncoding: "float32", DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds: []EmbeddingInputKind{EmbeddingInputOriginalFile}, CompatibilityID: contract.CompatibilityID,
		ModelInput: contract, SupportedRequestModes: []ModelInputMode{ModelInputModeText},
	})
	require.NoError(t, err)
	return descriptor
}

func testProofEmbeddingAuthorization(descriptor EmbeddingDescriptor) EmbeddingAuthorization {
	return EmbeddingAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 1,
		MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
	}
}
