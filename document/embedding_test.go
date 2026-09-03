package document_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// This test fails if execution accepts a response whose keys, order, or vector
// shape no longer exactly represent the authorized request.
func TestEmbeddingContractRejectsMalformedProviderResult(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	inputs := []document.EmbeddingInput{
		{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha"},
		{Key: "chunk-b", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "beta"},
	}
	authorization := testEmbeddingAuthorization(descriptor)

	for _, testCase := range []struct {
		name   string
		result document.EmbeddingResult
		want   string
	}{
		{"missing key", document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "chunk-a", Values: []float32{1, 2}}}}, "missing vector"},
		{"duplicate key", document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "chunk-a", Values: []float32{1, 2}}, {Key: "chunk-a", Values: []float32{3, 4}}}}, "duplicate vector key"},
		{"unexpected key", document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "chunk-a", Values: []float32{1, 2}}, {Key: "other", Values: []float32{3, 4}}}}, "unexpected vector key"},
		{"reordered without index", document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "chunk-b", Values: []float32{3, 4}}, {Key: "chunk-a", Values: []float32{1, 2}}}}, "vector order"},
		{"wrong dimensions", document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "chunk-a", Values: []float32{1}}, {Key: "chunk-b", Values: []float32{3, 4}}}}, "dimension"},
		{"non-finite", document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "chunk-a", Values: []float32{float32(math.NaN()), 2}}, {Key: "chunk-b", Values: []float32{3, 4}}}}, "non-finite"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := document.ValidateEmbeddingProviderResult(descriptor, inputs, authorization, testCase.result)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	indexed := document.EmbeddingResult{Vectors: []document.EmbeddingVector{
		{Key: "chunk-b", Index: new(1), Values: []float32{3, 4}},
		{Key: "chunk-a", Index: new(0), Values: []float32{1, 2}},
	}}
	require.NoError(t, document.ValidateEmbeddingProviderResult(descriptor, inputs, authorization, indexed))
	for _, testCase := range []struct {
		name   string
		result document.EmbeddingResult
		want   string
	}{
		{"partial indexes", document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "chunk-a", Index: new(0), Values: []float32{1, 2}}, {Key: "chunk-b", Values: []float32{3, 4}}}}, "partial indexes"},
		{"duplicate explicit zero", document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "chunk-a", Index: new(0), Values: []float32{1, 2}}, {Key: "chunk-b", Index: new(0), Values: []float32{3, 4}}}}, "duplicate vector index"},
		{"out of range index", document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "chunk-a", Index: new(2), Values: []float32{1, 2}}, {Key: "chunk-b", Index: new(1), Values: []float32{3, 4}}}}, "outside request bounds"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := document.ValidateEmbeddingProviderResult(descriptor, inputs, authorization, testCase.result)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	unsupported := append([]document.EmbeddingInput(nil), inputs...)
	unsupported[0].Role = document.EmbeddingRoleQuery
	err := document.ValidateEmbeddingProviderRequest(testEmbeddingProvider{descriptor: descriptor}, unsupported, authorization)
	require.ErrorContains(t, err, "unsupported role")
	unsupported = append([]document.EmbeddingInput(nil), inputs...)
	unsupported[0].Kind = document.EmbeddingInputOriginalFile
	err = document.ValidateEmbeddingProviderRequest(testEmbeddingProvider{descriptor: descriptor}, unsupported, authorization)
	require.ErrorContains(t, err, "unsupported kind")
}

// This test fails if document and query spaces can be treated as compatible
// after an identity-affecting descriptor difference.
func TestEmbeddingContractRejectsIncompatibleDocumentAndQueryDescriptors(t *testing.T) {
	documentDescriptor := testEmbeddingDescriptor(t)
	documentDescriptor.SupportsTextQuery = true
	documentDescriptor.Fingerprint = ""
	documentDescriptor, err := document.NewEmbeddingDescriptor(documentDescriptor)
	require.NoError(t, err)
	queryDescriptor := documentDescriptor
	queryDescriptor.Dimension++
	queryDescriptor.Fingerprint = ""
	queryDescriptor, err = document.NewEmbeddingDescriptor(queryDescriptor)
	require.NoError(t, err)

	err = document.ValidateEmbeddingQueryCompatibility(documentDescriptor, queryDescriptor)
	require.ErrorContains(t, err, "dimension")
}

func testEmbeddingDescriptor(t *testing.T) document.EmbeddingDescriptor {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileOpenAICompatible})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-embedder", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: testFingerprint(), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-model", ModelRevision: "r1", Dimension: 2, Metric: document.VectorMetricCosine,
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk},
		CompatibilityID: contract.CompatibilityID, ModelInput: contract,
		DocumentFormatter: "document/v1", QueryFormatter: "query/v1", Normalization: document.VectorNormalizationUnitLength,
		ScalarEncoding: "float32", SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	require.NoError(t, err)
	return descriptor
}

// This test fails if query eligibility is decided from a compatibility label
// while a distinct identity-bearing vector-space field has changed.
func TestEmbeddingContractRequiresExactQueryVectorSpace(t *testing.T) {
	base := testEmbeddingDescriptor(t)
	base.SupportsTextQuery = true
	base.Fingerprint = ""
	base, err := document.NewEmbeddingDescriptor(base)
	require.NoError(t, err)

	customInput, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: base.CompatibilityID,
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	for _, testCase := range []struct {
		name   string
		mutate func(*document.EmbeddingDescriptor)
		want   string
	}{
		{"model input", func(d *document.EmbeddingDescriptor) { d.ModelInput = customInput }, "model-input"},
		{"normalization", func(d *document.EmbeddingDescriptor) { d.Normalization = document.VectorNormalizationNone }, "normalization"},
		{"scalar encoding", func(d *document.EmbeddingDescriptor) { d.ScalarEncoding = "float16" }, "scalar encoding"},
		{"document formatter", func(d *document.EmbeddingDescriptor) { d.DocumentFormatter = "document/v2" }, "document formatter"},
		{"query formatter", func(d *document.EmbeddingDescriptor) { d.QueryFormatter = "query/v2" }, "query formatter"},
		{"input representation", func(d *document.EmbeddingDescriptor) {
			d.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
		}, "input representation"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := base
			testCase.mutate(&candidate)
			candidate.Fingerprint = ""
			candidate, err = document.NewEmbeddingDescriptor(candidate)
			require.NoError(t, err)
			err = document.ValidateEmbeddingQueryCompatibility(base, candidate)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

// This test fails if an adapter can silently gain request modes from a model
// contract it did not itself declare support for.
func TestEmbeddingContractRequiresExplicitProviderRequestModes(t *testing.T) {
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileOpenAICompatible})
	require.NoError(t, err)
	_, err = document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-embedder", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: testFingerprint(), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-model", ModelRevision: "r1", Dimension: 2, Metric: document.VectorMetricCosine,
		InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: contract.CompatibilityID,
		ModelInput: contract, DocumentFormatter: "document/v1", QueryFormatter: "query/v1", Normalization: document.VectorNormalizationUnitLength,
		ScalarEncoding: "float32",
	})
	require.ErrorContains(t, err, "must declare supported request modes")
}

// This test fails if the durable descriptor rejects the existing profile
// vocabulary or silently translates its normalization identifier.
func TestEmbeddingContractPreservesUnitLengthNormalization(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	descriptor.Normalization = document.VectorNormalizationUnitLength
	descriptor.Fingerprint = ""
	_, err := document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
}

// This test fails if a second normalization spelling can enter any durable
// contract boundary alongside the canonical unit_length value.
func TestEmbeddingContractRejectsL2NormalizationAlias(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	descriptor.Normalization = "l2"
	descriptor.Fingerprint = ""
	_, err := document.NewEmbeddingDescriptor(descriptor)
	require.ErrorContains(t, err, "normalization")
}

func TestEmbeddingContractEnforcesResultFootprint(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	input := []document.EmbeddingInput{{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha"}}
	authorization := testEmbeddingAuthorization(descriptor)
	authorization.MaxResponseBytes = 8
	calls := 0
	_, err := document.ExecuteEmbedding(context.Background(), countingEmbeddingProvider{descriptor: descriptor, calls: &calls}, input, authorization)
	require.ErrorContains(t, err, "response byte")
	require.Zero(t, calls)
}

// This test fails if original-file bytes can escape the validated upload
// length or if a successful provider result can survive a source-integrity
// failure.
func TestExecuteEmbeddingSealsOriginalFileUpload(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
	descriptor.Fingerprint = ""
	descriptor, err := document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
	authorized := []byte("source")

	for _, testCase := range []struct {
		name         string
		data         []byte
		sha256       string
		inputKind    document.RenditionInputKind
		wantProvider string
		wantErr      string
	}{
		{name: "exact", data: authorized, sha256: fmt.Sprintf("%x", sha256.Sum256(authorized)), wantProvider: "source"},
		{name: "short", data: []byte("sourc"), sha256: fmt.Sprintf("%x", sha256.Sum256(authorized)), wantProvider: "sourc", wantErr: "shorter"},
		{name: "long", data: []byte("source-extra"), sha256: fmt.Sprintf("%x", sha256.Sum256(authorized)), wantProvider: "source", wantErr: "exceeds"},
		{name: "wrong hash", data: authorized, sha256: fmt.Sprintf("%x", sha256.Sum256([]byte("target"))), wantProvider: "source", wantErr: "SHA-256"},
		{name: "rejected metadata", data: authorized, sha256: fmt.Sprintf("%x", sha256.Sum256(authorized)), inputKind: document.RenditionInputDerivedUpload, wantErr: "original file"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := testEmbeddingUploadMetadata(int64(len(authorized)))
			metadata.SHA256 = testCase.sha256
			if testCase.inputKind != "" {
				metadata.InputKind = testCase.inputKind
			}
			upload := &trackingEmbeddingUpload{Reader: bytes.NewReader(testCase.data), metadata: metadata}
			var providerBytes []byte
			provider := embeddingProviderFunc{descriptor: descriptor, embed: func(_ context.Context, inputs []document.EmbeddingInput, _ document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
				var readErr error
				providerBytes, readErr = io.ReadAll(inputs[0].Source)
				require.NoError(t, readErr)
				require.NoError(t, inputs[0].Source.Close())
				return document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "source", Values: []float32{1, 2}}}}, nil
			}}

			_, err := document.ExecuteEmbedding(t.Context(), provider, []document.EmbeddingInput{{
				Key: "source", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: upload,
			}}, testEmbeddingAuthorization(descriptor))
			if testCase.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, testCase.wantErr)
			}
			assert.Equal(t, testCase.wantProvider, string(providerBytes))
			assert.Equal(t, 1, upload.closes)
		})
	}
}

// This test fails if an original-file provider can read the source filename
// without an explicit disclosure grant in the authorization.
func TestExecuteEmbeddingWithholdsFilenameUnlessDisclosed(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
	descriptor.Fingerprint = ""
	descriptor, err := document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
	source := []byte("source")
	for _, testCase := range []struct {
		name     string
		disclose bool
		want     string
	}{
		{"withheld", false, ""},
		{"disclosed", true, "source.pdf"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := testEmbeddingUploadMetadata(int64(len(source)))
			metadata.SHA256 = fmt.Sprintf("%x", sha256.Sum256(source))
			upload := &trackingEmbeddingUpload{Reader: bytes.NewReader(source), metadata: metadata}
			var providerFilename string
			provider := embeddingProviderFunc{descriptor: descriptor, embed: func(_ context.Context, inputs []document.EmbeddingInput, _ document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
				providerFilename = inputs[0].Source.Metadata().Filename
				_, readErr := io.ReadAll(inputs[0].Source)
				require.NoError(t, readErr)
				return document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "source", Values: []float32{1, 2}}}}, nil
			}}
			authorization := testEmbeddingAuthorization(descriptor)
			authorization.DiscloseFilename = testCase.disclose

			_, err := document.ExecuteEmbedding(t.Context(), provider, []document.EmbeddingInput{{
				Key: "source", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: upload,
			}}, authorization)
			require.NoError(t, err)
			assert.Equal(t, testCase.want, providerFilename)
			assert.Equal(t, "source.pdf", upload.metadata.Filename, "caller metadata must stay intact")
		})
	}
}

// This test fails if provider-owned result buffers remain mutable after a
// successful embedding call returns.
func TestExecuteEmbeddingOwnsProviderResult(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	index := 0
	providerResult := document.EmbeddingResult{Vectors: []document.EmbeddingVector{{
		Key: "chunk-a", Index: &index, Values: []float32{1, 2},
	}}}
	provider := embeddingProviderFunc{descriptor: descriptor, embed: func(context.Context, []document.EmbeddingInput, document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
		return providerResult, nil
	}}
	result, err := document.ExecuteEmbedding(t.Context(), provider, []document.EmbeddingInput{{
		Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha",
	}}, testEmbeddingAuthorization(descriptor))
	require.NoError(t, err)

	providerResult.Vectors[0].Key = "reused"
	*providerResult.Vectors[0].Index = 1
	providerResult.Vectors[0].Values[0] = 99
	assert.Equal(t, "chunk-a", result.Vectors[0].Key)
	assert.Equal(t, 0, *result.Vectors[0].Index)
	assert.Equal(t, []float32{1, 2}, result.Vectors[0].Values)
}

// This test fails if a provider can rewrite the request slice it was handed
// and have its result accepted against that rewritten request.
func TestExecuteEmbeddingValidatesResultAgainstAuthorizedInputs(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	inputs := []document.EmbeddingInput{{
		Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha",
	}}
	provider := embeddingProviderFunc{descriptor: descriptor, embed: func(_ context.Context, providerInputs []document.EmbeddingInput, _ document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
		providerInputs[0].Key = "renamed"
		return document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "renamed", Values: []float32{1, 2}}}}, nil
	}}

	_, err := document.ExecuteEmbedding(t.Context(), provider, inputs, testEmbeddingAuthorization(descriptor))
	require.ErrorContains(t, err, "unexpected vector key")
	assert.Equal(t, "chunk-a", inputs[0].Key)
}

func TestEmbeddingContractRejectsInvalidDocumentAuxiliaryFields(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	provider := testEmbeddingProvider{descriptor: descriptor}
	authorization := testEmbeddingAuthorization(descriptor)
	for _, input := range []document.EmbeddingInput{
		{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", HeadingPath: []string{"\xff"}},
		{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", HeadingPath: []string{"heading\x00"}},
		{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", HeadingPath: []string{strings.Repeat("h", (1<<20)+1)}},
		{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", SourceSpans: []document.ChunkSpan{{UnitIndex: -1, CharStart: 0, CharEnd: 1}}},
		{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 2, CharEnd: 2}}},
		{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: (1 << 30) + 1}}},
	} {
		err := document.ValidateEmbeddingProviderRequest(provider, []document.EmbeddingInput{input}, authorization)
		require.Error(t, err)
	}
	require.NoError(t, document.ValidateEmbeddingProviderRequest(provider, []document.EmbeddingInput{{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", HeadingPath: []string{"Overview"}, SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 5}}}}, authorization))

	query := descriptor
	query.SupportsTextQuery = true
	query.Fingerprint = ""
	query, err := document.NewEmbeddingDescriptor(query)
	require.NoError(t, err)
	err = document.ValidateEmbeddingProviderRequest(testEmbeddingProvider{descriptor: query}, []document.EmbeddingInput{{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "alpha", HeadingPath: []string{"forbidden"}}}, testEmbeddingAuthorization(query))
	require.ErrorContains(t, err, "auxiliary")

	original := descriptor
	original.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
	original.Fingerprint = ""
	original, err = document.NewEmbeddingDescriptor(original)
	require.NoError(t, err)
	derived := testEmbeddingUploadMetadata(6)
	derived.InputKind = document.RenditionInputDerivedUpload
	err = document.ValidateEmbeddingProviderRequest(testEmbeddingProvider{descriptor: original}, []document.EmbeddingInput{{Key: "source", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: &testEmbeddingUpload{metadata: []document.AuthorizedUploadMetadata{derived}}, SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 1}}}}, testEmbeddingAuthorization(original))
	require.ErrorContains(t, err, "auxiliary")
	err = document.ValidateEmbeddingProviderRequest(testEmbeddingProvider{descriptor: original}, []document.EmbeddingInput{{Key: "source", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: &testEmbeddingUpload{metadata: []document.AuthorizedUploadMetadata{derived}}}}, testEmbeddingAuthorization(original))
	require.ErrorContains(t, err, "original file")
}

// This test fails if a span can name a source unit outside the canonical
// evidence-unit vocabulary and reach the provider boundary.
func TestEmbeddingContractRejectsOverLimitSourceUnitBeforeProviderCall(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	calls := 0
	_, err := document.ExecuteEmbedding(context.Background(), countingEmbeddingProvider{descriptor: descriptor, calls: &calls}, []document.EmbeddingInput{{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", SourceSpans: []document.ChunkSpan{{UnitIndex: 100_000, CharStart: 0, CharEnd: 1}}}}, testEmbeddingAuthorization(descriptor))
	require.ErrorContains(t, err, "source span")
	require.Zero(t, calls)
}

type countingEmbeddingProvider struct {
	descriptor document.EmbeddingDescriptor
	calls      *int
}

func (provider countingEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}
func (provider countingEmbeddingProvider) Embed(context.Context, []document.EmbeddingInput, document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	*provider.calls++
	return document.EmbeddingResult{}, nil
}

func TestEmbeddingContractRejectsUnrepresentableOrMutableInputs(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	provider := testEmbeddingProvider{descriptor: descriptor}
	authorization := testEmbeddingAuthorization(descriptor)
	typedNil := (*testEmbeddingUpload)(nil)
	err := document.ValidateEmbeddingProviderRequest(provider, []document.EmbeddingInput{{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", Source: typedNil}}, authorization)
	require.ErrorContains(t, err, "text embedding input")
	err = document.ValidateEmbeddingProviderRequest(provider, []document.EmbeddingInput{{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha", HeadingPath: make([]string, 1025)}}, authorization)
	require.ErrorContains(t, err, "auxiliary slices")

	withTemplate := descriptor
	withTemplate.ModelInput, err = document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: descriptor.CompatibilityID, Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "x{{content}}"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "{{content}}"}})
	require.NoError(t, err)
	withTemplate.Fingerprint = ""
	withTemplate, err = document.NewEmbeddingDescriptor(withTemplate)
	require.NoError(t, err)
	authorization = testEmbeddingAuthorization(withTemplate)
	authorization.MaxInputBytes = 5
	templateCalls := 0
	_, err = document.ExecuteEmbedding(context.Background(), countingEmbeddingProvider{descriptor: withTemplate, calls: &templateCalls}, []document.EmbeddingInput{{Key: "chunk-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha"}}, authorization)
	require.ErrorContains(t, err, "input bytes")
	require.Zero(t, templateCalls)

	original := descriptor
	original.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
	original.Fingerprint = ""
	original, err = document.NewEmbeddingDescriptor(original)
	require.NoError(t, err)
	provider = testEmbeddingProvider{descriptor: original}
	authorization = testEmbeddingAuthorization(original)
	metadata := testEmbeddingUploadMetadata(6)
	err = document.ValidateEmbeddingProviderRequest(provider, []document.EmbeddingInput{{Key: "source", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Text: "unexpected", Source: &testEmbeddingUpload{metadata: []document.AuthorizedUploadMetadata{metadata}}}}, authorization)
	require.ErrorContains(t, err, "original-file")

	authorization.MaxInputBytes = 5
	err = document.ValidateEmbeddingProviderRequest(provider, []document.EmbeddingInput{{Key: "source", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: &testEmbeddingUpload{metadata: []document.AuthorizedUploadMetadata{metadata}}}}, authorization)
	require.ErrorContains(t, err, "input bytes")

	changed := metadata
	changed.Filename = "changed.pdf"
	authorization.MaxInputBytes = 1 << 20
	err = document.ValidateEmbeddingProviderRequest(provider, []document.EmbeddingInput{{Key: "source", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: &testEmbeddingUpload{metadata: []document.AuthorizedUploadMetadata{metadata, changed}}}}, authorization)
	require.ErrorContains(t, err, "changed during validation")
}

type testEmbeddingUpload struct {
	metadata []document.AuthorizedUploadMetadata
	reads    int
}

type trackingEmbeddingUpload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
	closes   int
}

func (upload *trackingEmbeddingUpload) Close() error {
	upload.closes++
	return nil
}
func (upload *trackingEmbeddingUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload.metadata
}

func (upload *testEmbeddingUpload) Read(data []byte) (int, error) { return 0, io.EOF }
func (upload *testEmbeddingUpload) Close() error                  { return nil }
func (upload *testEmbeddingUpload) Metadata() document.AuthorizedUploadMetadata {
	result := upload.metadata[min(upload.reads, len(upload.metadata)-1)]
	upload.reads++
	return result
}

func testEmbeddingUploadMetadata(size int64) document.AuthorizedUploadMetadata {
	return document.AuthorizedUploadMetadata{Filename: "source.pdf", MediaFamily: "pdf", MediaType: "application/pdf", ByteLength: size, SHA256: strings.Repeat("a", 64), CapabilityRecordChecksum: strings.Repeat("b", 64), ProviderMetadataChecksum: strings.Repeat("c", 64), InputKind: document.RenditionInputOriginalFile}
}

func testEmbeddingAuthorization(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 10, MaxInputBytes: 1 << 20,
		MaxResponseBytes: 1 << 20,
	}
}

type testEmbeddingProvider struct{ descriptor document.EmbeddingDescriptor }

func (provider testEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}

func (testEmbeddingProvider) Embed(context.Context, []document.EmbeddingInput, document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	return document.EmbeddingResult{}, nil
}

type embeddingProviderFunc struct {
	descriptor document.EmbeddingDescriptor
	embed      func(context.Context, []document.EmbeddingInput, document.EmbeddingAuthorization) (document.EmbeddingResult, error)
}

func (provider embeddingProviderFunc) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}

func (provider embeddingProviderFunc) Embed(
	ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	return provider.embed(ctx, inputs, authorization)
}

func testFingerprint() string {
	// A deterministic lowercase SHA-256 fixture, deliberately not calculated by
	// the contract under test.
	return "823412d1eacb67956220e532c5e1c72ebf4d55d7ac7c5a9686507b54b7c5cfcc"
}

func TestEmbeddingContractExecutesOnlyValidatedRequests(t *testing.T) {
	descriptor := testEmbeddingDescriptor(t)
	provider := testEmbeddingProvider{descriptor: descriptor}
	_, err := document.ExecuteEmbedding(context.Background(), provider, []document.EmbeddingInput{{
		Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "alpha",
	}}, testEmbeddingAuthorization(descriptor))
	assert.ErrorContains(t, err, "unsupported role")
}
