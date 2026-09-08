package geminiembed

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestTextExecutionOwnsCallerInputsAcrossSecretCallback(t *testing.T) {
	for _, transport := range []Transport{TransportInline, TransportFilesAPI} {
		t.Run(string(transport), func(t *testing.T) {
			profile := geminiTestProfile(t, 128)
			profile.Transport = transport
			profile.Descriptor = geminiDescriptorFor(t, profile)
			inputs := []document.EmbeddingInput{{
				Key: "document-1", Role: document.EmbeddingRoleDocument,
				Kind: document.EmbeddingInputRenditionChunk, Text: "passage",
				HeadingPath: []string{"Synthetic heading"},
				SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 7}},
			}}
			secrets := callbackSecrets{callback: func() {
				inputs[0].Key = "replacement"
				inputs[0].Text = "replacement"
				inputs[0].HeadingPath[0] = ""
				inputs[0].SourceSpans[0].CharEnd = 0
			}}
			client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
			}))

			result, err := client.Embed(t.Context(), inputs, geminiAuthorization(profile.Descriptor, 1))
			require.NoError(t, err)
			require.Len(t, result.Vectors, 1)
			assert.Equal(t, "document-1", result.Vectors[0].Key)
		})
	}
}

func TestInvalidAuxiliaryAndOriginalFileFailBeforeSecretOrHTTP(t *testing.T) {
	original := &unreadUpload{metadata: document.AuthorizedUploadMetadata{
		MediaFamily: "image", MediaType: "image/png", ByteLength: 1,
		SHA256: strings.Repeat("a", 64), CapabilityRecordChecksum: strings.Repeat("b", 64),
		ProviderMetadataChecksum: strings.Repeat("c", 64), InputKind: document.RenditionInputOriginalFile,
	}}
	tests := []struct {
		name  string
		input document.EmbeddingInput
	}{
		{name: "invalid auxiliary", input: document.EmbeddingInput{
			Key: "document", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk,
			Text: "passage", HeadingPath: []string{""},
		}},
		{name: "original file", input: document.EmbeddingInput{
			Key: "original", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: original,
		}},
	}
	for _, test := range tests {
		for _, transport := range []Transport{TransportInline, TransportFilesAPI} {
			t.Run(test.name+"/"+string(transport), func(t *testing.T) {
				profile := geminiTestProfile(t, 128)
				profile.Transport = transport
				profile.Descriptor = geminiDescriptorFor(t, profile)
				secrets := &countingSecrets{value: "synthetic-key"}
				var requests atomic.Int32
				client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
					requests.Add(1)
					return nil, errors.New("unexpected egress")
				}))

				execution, err := client.EmbedWithReceipt(t.Context(), []document.EmbeddingInput{test.input}, geminiAuthorization(profile.Descriptor, 1))
				require.Error(t, err)
				assert.Empty(t, execution.Result)
				assert.Zero(t, secrets.calls.Load())
				assert.Zero(t, requests.Load())
			})
		}
	}
	assert.Zero(t, original.reads.Load())
}

func TestVerifiedOriginalFileClientRoutingSplitsInlineAndFilesAPI(t *testing.T) {
	data := mediatest.PNG(2, 2, nil)

	t.Run("Inline executes with receipt", func(t *testing.T) {
		profile := geminiTestProfile(t, 128)
		profile.Descriptor = geminiDescriptorFor(t, profile)
		metadata, proof := issuedGeminiAuthority(t, profile, data, "inline.png", "image/png")
		source := newProofUpload(data, metadata, proof)
		secrets := &countingSecrets{value: "synthetic-key"}
		var requests atomic.Int32
		client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			response := geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`)
			response.Header.Set("X-Goog-Request-Id", "inline-route")
			return response, nil
		}))

		execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

		require.NoError(t, err)
		require.Len(t, execution.Result.Vectors, 1)
		assert.Equal(t, "direct-a", execution.Result.Vectors[0].Key)
		assert.Len(t, execution.Result.Vectors[0].Values, 128)
		assert.InDelta(t, 1, execution.Result.Vectors[0].Values[0], 1e-6)
		assert.Equal(t, ProviderID, execution.Receipt.ProviderID)
		assert.Equal(t, profile.Descriptor.Fingerprint, execution.Receipt.DescriptorFingerprint)
		assert.Equal(t, TransportInline, execution.Receipt.Transport)
		assert.Equal(t, 1, execution.Receipt.RequestCount)
		assert.Equal(t, 1, execution.Receipt.EmbeddingResponseCount)
		assert.Zero(t, execution.Receipt.UsageResponseCount)
		assert.Equal(t, []string{"inline-route"}, execution.Receipt.ProviderResponseIDs)
		assert.Equal(t, int32(1), source.readPasses.Load())
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Equal(t, int32(1), secrets.calls.Load())
		assert.Equal(t, int32(1), requests.Load())
	})

	t.Run("Files API executes full lifecycle with confirmed cleanup receipt", func(t *testing.T) {
		profile := geminiTestProfile(t, 128)
		profile.Transport = TransportFilesAPI
		profile.Descriptor = geminiDescriptorFor(t, profile)
		metadata, proof := issuedGeminiAuthority(t, profile, data, "files.png", "image/png")
		source := newProofUpload(data, metadata, proof)
		secrets := &countingSecrets{value: "synthetic-key"}
		lifecycle := newSuccessfulFilesLifecycle(t, data, "image/png", "files.png")
		client := newGeminiTestClient(t, profile, secrets, lifecycle)

		execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

		require.NoError(t, err)
		require.Len(t, execution.Result.Vectors, 1)
		assert.Equal(t, "direct-a", execution.Result.Vectors[0].Key)
		assert.Len(t, execution.Result.Vectors[0].Values, 128)
		assert.Equal(t, ProviderID, execution.Receipt.ProviderID)
		assert.Equal(t, profile.Descriptor.Fingerprint, execution.Receipt.DescriptorFingerprint)
		assert.Equal(t, profile.Descriptor.PolicyFingerprint, execution.Receipt.PolicyFingerprint)
		assert.Equal(t, TransportFilesAPI, execution.Receipt.Transport)
		assert.Equal(t, fixedSemantics(), execution.Receipt.Semantics)
		assert.Equal(t, retentionCeiling, execution.Receipt.ProviderRetentionCeiling)
		assert.Zero(t, execution.Receipt.UnconfirmedFileRetentions)
		assert.Equal(t, 6, execution.Receipt.RequestCount)
		assert.Equal(t, 1, execution.Receipt.EmbeddingResponseCount)
		assert.Zero(t, execution.Receipt.UsageResponseCount)
		assert.Empty(t, execution.Receipt.ProviderResponseIDs)
		assert.Empty(t, execution.Receipt.Warnings)
		assert.Equal(t, int32(1), source.readPasses.Load())
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Equal(t, int32(1), secrets.calls.Load())
		assert.Len(t, lifecycle.snapshot(), 6)
	})
}

func TestAllTextRequestsFitCapacityBeforeSecretOrHTTP(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.MaxRequestBytes = int64(len(`{"model":"models/gemini-embedding-2","content":{"parts":[{"text":"title: none | text: a"}]},"outputDimensionality":128}`))
	profile.Descriptor = geminiDescriptorFor(t, profile)
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected egress")
	}))
	inputs := []document.EmbeddingInput{
		{Key: "short", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "a"},
		{Key: "large", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: strings.Repeat("x", 100)},
	}

	_, err := client.Embed(t.Context(), inputs, geminiAuthorization(profile.Descriptor, 2))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request")
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

type callbackSecrets struct{ callback func() }

func (resolver callbackSecrets) ResolveSecret(context.Context, string) (string, error) {
	resolver.callback()
	return "synthetic-key", nil
}

type countingSecrets struct {
	value string
	calls atomic.Int32
}

type unreadUpload struct {
	metadata document.AuthorizedUploadMetadata
	reads    atomic.Int32
}

func (upload *unreadUpload) Read([]byte) (int, error) {
	upload.reads.Add(1)
	return 0, io.EOF
}

func (*unreadUpload) Close() error { return nil }

func (upload *unreadUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

func (resolver *countingSecrets) ResolveSecret(context.Context, string) (string, error) {
	resolver.calls.Add(1)
	return resolver.value, nil
}

func newGeminiTestClient(t *testing.T, profile Profile, secrets SecretResolver, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := New(profile, secrets, syntheticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)
	client.http.Transport = transport
	return client
}

func geminiAuthorization(descriptor document.EmbeddingDescriptor, items int) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: items,
		MaxInputBytes: 4096, MaxResponseBytes: 16384,
	}
}
