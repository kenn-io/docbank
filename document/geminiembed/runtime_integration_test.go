package geminiembed_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/geminiembed"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestGeminiRuntimeExecutesInspectedPNGThroughCoreAndAdapter(t *testing.T) {
	data := mediatest.PNG(2, 2, nil)
	client, descriptor, disclosure, secrets, requests, bodies := runtimeGeminiClient(t, geminiembed.TransportInline)
	record, binding := runtimeGeminiProfile(t, descriptor, disclosure, nil)
	blobs := &runtimeBlobs{data: data}
	spool := t.TempDir()
	runtime, err := processing.NewProviderEmbeddingRuntime(client, blobs, spool,
		func(error) (processing.EmbeddingProviderFailure, time.Duration) {
			return processing.EmbeddingProviderTransient, time.Second
		})
	require.NoError(t, err)
	work := runtimeGeminiWork(data, "synthetic.png", "image/png", record, binding, descriptor)

	execution, err := runtime.Prepare(t.Context(), work)
	require.NoError(t, err)
	require.Len(t, execution.Inputs, 1)
	result, err := document.ExecuteEmbedding(t.Context(), execution.Provider, execution.Inputs,
		document.EmbeddingAuthorization{
			ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
			PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 1,
			MaxInputBytes: binding.MaxInputBytes, MaxResponseBytes: binding.MaxResponseBytes,
		})

	require.NoError(t, err)
	require.Len(t, result.Vectors, 1)
	assert.Equal(t, work.ContentVersionID, result.Vectors[0].Key)
	assert.Equal(t, int32(1), secrets.Load())
	assert.Equal(t, int32(1), requests.Load())
	assert.Contains(t, string(*bodies), base64.StdEncoding.EncodeToString(data))
	assert.NotContains(t, string(*bodies), work.SourceFilename)
	assert.Equal(t, 1, blobs.opens)
	assert.Equal(t, int32(1), blobs.closes.Load())
	entries, err := os.ReadDir(spool)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestGeminiRuntimeExecutesAllSupportedMediaAndMeasuredFrames(t *testing.T) {
	tests := []struct {
		name, filename, mediaType string
		data                      []byte
	}{
		{"PNG", "synthetic.png", "image/png", mediatest.PNG(2, 3, nil)},
		{"JPEG", "synthetic.jpg", "image/jpeg", mediatest.JPEG(3, 2, nil)},
		{"PDF six pages", "synthetic.pdf", "application/pdf", runtimePDFPages(6)},
		{"MP3", "synthetic.mp3", "audio/mpeg", mediatest.MP3()},
		{"WAV", "synthetic.wav", "audio/wav", runtimeWAV(8_000, 80)},
		{"H264 MOV", "synthetic.mov", "video/quicktime", mediatest.H264MOV()},
		{"H265 MP4", "synthetic.mp4", "video/mp4", mediatest.H265MP4()},
		{"VP9 MP4", "synthetic.mp4", "video/mp4", mediatest.VP9MP4()},
		{"AV1 MP4", "synthetic.mp4", "video/mp4", mediatest.AV1MP4()},
		{"H264 measured 33 frames", "synthetic.mp4", "video/mp4", runtimeMappedAVCMP4(33, 33_000)},
	}
	for _, test := range tests {
		for _, disclose := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disclose=%t", test.name, disclose), func(t *testing.T) {
				client, descriptor, disclosure, secrets, requests, bodies := runtimeGeminiClient(t, geminiembed.TransportInline)
				record, binding := runtimeGeminiProfile(t, descriptor, disclosure, nil)
				blobs := &runtimeBlobs{data: test.data}
				spool := t.TempDir()
				runtime, err := processing.NewProviderEmbeddingRuntime(client, blobs, spool,
					func(error) (processing.EmbeddingProviderFailure, time.Duration) {
						return processing.EmbeddingProviderTransient, time.Second
					})
				require.NoError(t, err)
				work := runtimeGeminiWork(test.data, test.filename, test.mediaType, record, binding, descriptor)
				execution, err := runtime.Prepare(t.Context(), work)
				require.NoError(t, err)
				require.Len(t, execution.Inputs, 1)
				assert.Equal(t, test.filename, execution.Inputs[0].Source.Metadata().Filename,
					"local inspection must retain the source filename")
				_, err = document.ExecuteEmbedding(t.Context(), execution.Provider, execution.Inputs,
					document.EmbeddingAuthorization{
						ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
						PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 1,
						MaxInputBytes: binding.MaxInputBytes, MaxResponseBytes: binding.MaxResponseBytes,
						DiscloseFilename: disclose,
					})
				require.NoError(t, err)
				assert.Equal(t, int32(1), secrets.Load())
				assert.Equal(t, int32(1), requests.Load())
				assert.Contains(t, string(*bodies), base64.StdEncoding.EncodeToString(test.data))
				assert.NotContains(t, string(*bodies), test.filename)
				assert.NotContains(t, string(*bodies), "filename")
				assert.Equal(t, int32(1), blobs.closes.Load())
				entries, err := os.ReadDir(spool)
				require.NoError(t, err)
				assert.Empty(t, entries)
			})
		}
	}
}

func TestGeminiRuntimeFilesLifecycleConfirmsCleanup(t *testing.T) {
	data := mediatest.PNG(2, 2, nil)
	client, descriptor, disclosure, secrets, _, _ := runtimeGeminiClient(t, geminiembed.TransportFilesAPI)
	lifecycle := &runtimeFilesLifecycle{t: t, data: data, mediaType: "image/png"}
	geminiembed.SetEmbeddingTestTransport(client, lifecycle)
	record, binding := runtimeGeminiProfile(t, descriptor, disclosure, nil)
	blobs := &runtimeBlobs{data: data}
	spool := t.TempDir()
	runtime, err := processing.NewProviderEmbeddingRuntime(client, blobs, spool,
		func(error) (processing.EmbeddingProviderFailure, time.Duration) {
			return processing.EmbeddingProviderTransient, time.Second
		})
	require.NoError(t, err)
	work := runtimeGeminiWork(data, "synthetic.png", "image/png", record, binding, descriptor)
	execution, err := runtime.Prepare(t.Context(), work)
	require.NoError(t, err)

	_, err = document.ExecuteEmbedding(t.Context(), execution.Provider, execution.Inputs,
		document.EmbeddingAuthorization{
			ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
			PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 1,
			MaxInputBytes: binding.MaxInputBytes, MaxResponseBytes: binding.MaxResponseBytes,
		})
	require.NoError(t, err)
	assert.Equal(t, int32(1), secrets.Load())
	assert.Equal(t, int32(6), lifecycle.requests.Load())
	assert.True(t, lifecycle.deleted.Load())
	assert.NotContains(t, lifecycle.bodies.String(), work.SourceFilename)
	assert.Equal(t, int32(1), blobs.closes.Load())
	entries, err := os.ReadDir(spool)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestGeminiRuntimeRejectsMeasuredAndDeclaredPolicyOverflowBeforeEgress(t *testing.T) {
	tests := []struct {
		name, filename, mediaType string
		data                      []byte
	}{
		{"PDF seven pages", "synthetic.pdf", "application/pdf", runtimePDFPages(7)},
		{"audio duration plus one", "synthetic.wav", "audio/wav", runtimeWAV(1_000, 180_001)},
		{"video duration plus one", "synthetic.mp4", "video/mp4", runtimeMappedAVCMP4(1, 120_001)},
		{"declared audio cannot authorize video", "synthetic.mp4", "audio/mpeg", runtimeMappedAVCMP4(1, 120_001)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, descriptor, disclosure, secrets, requests, _ := runtimeGeminiClient(t, geminiembed.TransportInline)
			record, binding := runtimeGeminiProfile(t, descriptor, disclosure, nil)
			blobs := &runtimeBlobs{data: test.data}
			spool := t.TempDir()
			runtime, err := processing.NewProviderEmbeddingRuntime(client, blobs, spool,
				func(error) (processing.EmbeddingProviderFailure, time.Duration) {
					return processing.EmbeddingProviderTransient, time.Second
				})
			require.NoError(t, err)
			work := runtimeGeminiWork(test.data, test.filename, test.mediaType, record, binding, descriptor)

			_, err = runtime.Prepare(t.Context(), work)
			require.Error(t, err)
			assert.Equal(t, 1, blobs.opens)
			assert.Equal(t, int32(1), blobs.closes.Load())
			assert.Zero(t, secrets.Load())
			assert.Zero(t, requests.Load())
			entries, err := os.ReadDir(spool)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestGeminiRuntimeRejectsAuthorityTransplantsBeforeSourceOrEgress(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *processing.EmbeddingWork, document.EmbeddingDescriptor, string)
	}{
		{"full profile", func(_ *testing.T, work *processing.EmbeddingWork, _ document.EmbeddingDescriptor, _ string) {
			work.ProcessingProfile.Fingerprint = runtimeHash("transplanted-profile")
		}},
		{"binding", func(_ *testing.T, work *processing.EmbeddingWork, _ document.EmbeddingDescriptor, _ string) {
			work.Binding.MaxBatchItems++
		}},
		{"descriptor", func(_ *testing.T, work *processing.EmbeddingWork, _ document.EmbeddingDescriptor, _ string) {
			work.Descriptor.Fingerprint = runtimeHash("transplanted-descriptor")
		}},
		{"disclosure", func(t *testing.T, work *processing.EmbeddingWork, descriptor document.EmbeddingDescriptor, _ string) {
			t.Helper()
			record, binding := runtimeGeminiProfile(t, descriptor, runtimeHash("transplanted-disclosure"), nil)
			work.ProcessingProfile, work.Binding = record, binding
		}},
		{"byte capacity plus one", func(_ *testing.T, work *processing.EmbeddingWork, _ document.EmbeddingDescriptor, _ string) {
			work.SourceBytes = work.Binding.MaxInputBytes + 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := mediatest.PNG(2, 2, nil)
			client, descriptor, disclosure, secrets, requests, _ := runtimeGeminiClient(t, geminiembed.TransportInline)
			record, binding := runtimeGeminiProfile(t, descriptor, disclosure, nil)
			work := runtimeGeminiWork(data, "synthetic.png", "image/png", record, binding, descriptor)
			test.mutate(t, &work, descriptor, disclosure)
			blobs := &runtimeBlobs{data: data}
			runtime, err := processing.NewProviderEmbeddingRuntime(client, blobs, t.TempDir(),
				func(error) (processing.EmbeddingProviderFailure, time.Duration) {
					return processing.EmbeddingProviderTransient, time.Second
				})
			require.NoError(t, err)

			_, err = runtime.Prepare(t.Context(), work)
			require.Error(t, err)
			assert.Zero(t, blobs.opens)
			assert.Zero(t, secrets.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

func TestGeminiRuntimeProfilePersistenceCeiling(t *testing.T) {
	client, descriptor, disclosure, secrets, requests, _ := runtimeGeminiClient(t, geminiembed.TransportInline)
	data := mediatest.PNG(2, 2, nil)

	t.Run("Store-admissible 64 binding profile", func(t *testing.T) {
		extra := runtimeCustomBindings(t, 63, 2_048)
		record, binding := runtimeGeminiProfile(t, descriptor, disclosure, extra)
		require.LessOrEqual(t, len(record.CanonicalProfile), 1<<20)
		require.NoError(t, store.ValidateProcessingProfileRecord(record))
		blobs := &runtimeBlobs{data: data}
		spool := t.TempDir()
		runtime, err := processing.NewProviderEmbeddingRuntime(client, blobs, spool,
			func(error) (processing.EmbeddingProviderFailure, time.Duration) {
				return processing.EmbeddingProviderTransient, time.Second
			})
		require.NoError(t, err)
		execution, err := runtime.Prepare(t.Context(), runtimeGeminiWork(data, "synthetic.png", "image/png", record, binding, descriptor))
		require.NoError(t, err)
		require.NoError(t, execution.Inputs[0].Source.Close())
		assert.Equal(t, 1, blobs.opens)
		assert.Equal(t, int32(1), blobs.closes.Load())
		entries, err := os.ReadDir(spool)
		require.NoError(t, err)
		assert.Empty(t, entries)
		assert.Zero(t, secrets.Load())
		assert.Zero(t, requests.Load())
	})

	t.Run("document-valid oversized profile", func(t *testing.T) {
		extra := runtimeCustomBindings(t, 63, 4_096)
		record, binding := runtimeGeminiProfileUnchecked(t, descriptor, disclosure, extra)
		require.Greater(t, len(record.CanonicalProfile), 1<<20)
		require.Error(t, store.ValidateProcessingProfileRecord(record))
		blobs := &runtimeBlobs{data: data}
		runtime, err := processing.NewProviderEmbeddingRuntime(client, blobs, t.TempDir(),
			func(error) (processing.EmbeddingProviderFailure, time.Duration) {
				return processing.EmbeddingProviderTransient, time.Second
			})
		require.NoError(t, err)
		_, err = runtime.Prepare(t.Context(), runtimeGeminiWork(data, "synthetic.png", "image/png", record, binding, descriptor))
		require.Error(t, err)
		assert.Zero(t, blobs.opens)
		assert.Zero(t, secrets.Load())
		assert.Zero(t, requests.Load())
	})
}

func runtimeGeminiClient(t *testing.T, transport geminiembed.Transport) (*geminiembed.Client,
	document.EmbeddingDescriptor, string, *atomic.Int32, *atomic.Int32, *[]byte,
) {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "gemini-embedding-2/search/v1",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "title: none | text: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "task: search result | query: {{content}}"},
	})
	require.NoError(t, err)
	disclosure := runtimeHash("gemini-disclosure")
	profile := geminiembed.Profile{
		CompatibilityEpoch: "epoch-1", SecretBinding: "secret:gemini", Transport: transport,
		CapabilityProfileFingerprint: runtimeHash("gemini-capability"), DisclosureFingerprint: disclosure,
		RequestTimeout: time.Second, PollInterval: 10 * time.Millisecond, MaxPollAttempts: 3,
		CleanupTimeout: time.Second, MaxInputBytes: 1 << 20, MaxRequestBytes: 2 << 20, MaxResponseBytes: 1 << 20,
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "https", Host: "generativelanguage.googleapis.com", Port: 443,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, ProxyMode: providerhttp.ProxyDisabled,
			ConnectTimeout: time.Second, KeepAlive: time.Second, TLSHandshakeTimeout: time.Second,
		},
		Descriptor: document.EmbeddingDescriptor{
			ID: geminiembed.ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			TrustBoundary: document.EmbeddingTrustHostedProvider, Model: geminiembed.Model,
			ModelRevision: "epoch-1", Dimension: 128, Metric: document.VectorMetricCosine,
			Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: geminiembed.ScalarEncodingFloat32,
			DocumentFormatter: geminiembed.DocumentFormatterV1, QueryFormatter: geminiembed.QueryFormatterV1,
			InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk},
			CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
		},
	}
	policyFingerprint, err := geminiembed.PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = policyFingerprint
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	secrets := new(atomic.Int32)
	requests := new(atomic.Int32)
	bodies := new([]byte)
	client, err := geminiembed.New(profile, runtimeSecrets{calls: secrets}, runtimeResolver{}, &http.Client{})
	require.NoError(t, err)
	geminiembed.SetEmbeddingTestTransport(client, runtimeRoundTrip(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			return nil, readErr
		}
		*bodies = append(*bodies, body...)
		values := make([]float32, 128)
		values[0] = 1
		encoded, encodeErr := json.Marshal(struct {
			Embedding struct {
				Values []float32 `json:"values"`
			} `json:"embedding"`
		}{Embedding: struct {
			Values []float32 `json:"values"`
		}{Values: values}})
		if encodeErr != nil {
			return nil, encodeErr
		}
		return &http.Response{StatusCode: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(bytes.NewReader(encoded)), Request: request}, nil
	}))
	return client, profile.Descriptor, disclosure, secrets, requests, bodies
}

func runtimeGeminiProfile(t *testing.T, descriptor document.EmbeddingDescriptor, disclosure string,
	extra []document.EmbeddingBindingV1,
) (store.ProcessingProfileRecord, document.EmbeddingBindingV1) {
	t.Helper()
	record, binding := runtimeGeminiProfileUnchecked(t, descriptor, disclosure, extra)
	require.NoError(t, store.ValidateProcessingProfileRecord(record))
	return record, binding
}

func runtimeGeminiProfileUnchecked(t *testing.T, descriptor document.EmbeddingDescriptor, disclosure string,
	extra []document.EmbeddingBindingV1,
) (store.ProcessingProfileRecord, document.EmbeddingBindingV1) {
	t.Helper()
	binding := document.EmbeddingBindingV1{
		Activation: document.EmbeddingOptional, AuthorizationFingerprint: runtimeHash("gemini-authorization"),
		CompatibilityID: descriptor.CompatibilityID, CredentialBinding: "credential:gemini",
		Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
		Dimensions: descriptor.Dimension, DisclosureFingerprint: disclosure,
		DocumentFormatter: descriptor.DocumentFormatter, InputKind: document.EmbeddingInputOriginalFile,
		MaxBatchItems: 1, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		Metric: descriptor.Metric, ModelInput: descriptor.ModelInput, Model: descriptor.Model,
		Name: "gemini", Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
		ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
	}
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Embeddings:      append([]document.EmbeddingBindingV1{binding}, extra...),
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			MaxDocumentChars: 10_000, CompletenessFingerprint: runtimeHash("complete"),
			LexicalSegmenterFingerprint: runtimeHash("lexical"), MaxSegmentRunes: 1_000, MaxUnitRunes: 1_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      runtimeHash("normalizer"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: runtimeHash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: runtimeHash("attachment"), ConsentFingerprint: runtimeHash("consent"),
			TrustBoundary: string(document.EmbeddingTrustHostedProvider),
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 10, VectorLimit: 10},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	record := store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: canonical,
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
	return record, binding
}

func runtimeCustomBindings(t *testing.T, count, templateBytes int) []document.EmbeddingBindingV1 {
	t.Helper()
	bindings := make([]document.EmbeddingBindingV1, 0, count)
	for index := range count {
		compatibility := fmt.Sprintf("synthetic-space-%02d", index)
		template := strings.Repeat("\\", templateBytes-len("{{content}}")) + "{{content}}"
		contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
			Profile: document.ModelInputProfileCustom, CompatibilityID: compatibility,
			Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: template},
			Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: template},
		})
		require.NoError(t, err)
		bindings = append(bindings, document.EmbeddingBindingV1{
			Activation: document.EmbeddingOptional, AuthorizationFingerprint: runtimeHash(fmt.Sprintf("custom-auth-%02d", index)),
			CompatibilityID: compatibility, CredentialBinding: "credential:synthetic",
			Descriptor: document.ProviderDescriptorV1{ID: fmt.Sprintf("synthetic-%02d", index), Fingerprint: runtimeHash(fmt.Sprintf("custom-descriptor-%02d", index))},
			Dimensions: 128, DisclosureFingerprint: runtimeHash(fmt.Sprintf("custom-disclosure-%02d", index)),
			DocumentFormatter: "synthetic-document/v1", InputKind: document.EmbeddingInputOriginalFile,
			MaxBatchItems: 1, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
			Metric: document.VectorMetricCosine, ModelInput: contract, Model: "synthetic-model",
			Name: fmt.Sprintf("custom-%02d", index), Normalization: document.VectorNormalizationUnitLength,
			QueryFormatter: "synthetic-query/v1", ScalarEncoding: "float32",
			TrustBoundary: string(document.EmbeddingTrustHostedProvider),
		})
	}
	return bindings
}

func runtimeGeminiWork(data []byte, filename, mediaType string, profile store.ProcessingProfileRecord,
	binding document.EmbeddingBindingV1, descriptor document.EmbeddingDescriptor,
) processing.EmbeddingWork {
	versionID := "00000000-0000-4000-8000-000000000001"
	return processing.EmbeddingWork{
		ContentVersionID: versionID, ProcessingProfile: profile, Binding: binding, Descriptor: descriptor,
		SourceBlobHash: runtimeHashBytes(data), SourceBytes: int64(len(data)),
		SourceFilename: filename, SourceMediaType: mediaType,
		InputGeneration: store.EmbeddingInputGenerationRecord{Inputs: []store.EmbeddingInputReference{{ID: versionID}}},
	}
}

type runtimeSecrets struct{ calls *atomic.Int32 }

func (secrets runtimeSecrets) ResolveSecret(context.Context, string) (string, error) {
	secrets.calls.Add(1)
	return "synthetic-key", nil
}

type runtimeResolver struct{}

func (runtimeResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
}

type runtimeRoundTrip func(*http.Request) (*http.Response, error)

func (roundTrip runtimeRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type runtimeBlobs struct {
	data   []byte
	opens  int
	closes atomic.Int32
}

func (blobs *runtimeBlobs) OpenContext(context.Context, string) (io.ReadSeekCloser, error) {
	blobs.opens++
	return &runtimeReader{Reader: bytes.NewReader(slices.Clone(blobs.data)), closes: &blobs.closes}, nil
}

type runtimeReader struct {
	*bytes.Reader

	closes *atomic.Int32
}

func (reader *runtimeReader) Close() error {
	reader.closes.Add(1)
	return nil
}

func runtimeHash(value string) string { return runtimeHashBytes([]byte(value)) }

func runtimeHashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func runtimePDFPages(pageCount int) []byte {
	kids := make([]string, 0, pageCount)
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	for index := range pageCount {
		kids = append(kids, fmt.Sprintf("%d 0 R", index+3))
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>")
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pageCount)
	var output bytes.Buffer
	_, _ = output.WriteString("%PDF-1.4\n%synthetic\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

func runtimeWAV(byteRate, dataBytes uint32) []byte {
	data := make([]byte, 44+dataBytes)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], byteRate)
	binary.LittleEndian.PutUint32(data[28:32], byteRate)
	binary.LittleEndian.PutUint16(data[32:34], 1)
	binary.LittleEndian.PutUint16(data[34:36], 8)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], dataBytes)
	return data
}

func runtimeMappedAVCMP4(count int, durationMS int64) []byte {
	sample := mediatest.H264PictureSample()
	ftyp := mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...))
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1_000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(durationMS))
	track := runtimeMappedAVCTrack(count, len(sample), durationMS)
	moov := mediatest.Box("moov", slices.Concat(mediatest.Box("mvhd", mvhd), track))
	offset := len(ftyp) + len(moov) + 8
	data := slices.Concat(ftyp, moov, mediatest.Box("mdat", bytes.Repeat(sample, count)))
	stco := runtimeMP4BoxPayload(data, "stco")
	binary.BigEndian.PutUint32(stco[8:12], uint32(offset))
	return data
}

func runtimeMappedAVCTrack(count, sampleSize int, durationMS int64) []byte {
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[20:24], uint32(durationMS))
	binary.BigEndian.PutUint32(tkhd[76:80], 16<<16)
	binary.BigEndian.PutUint32(tkhd[80:84], 16<<16)
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:26], 16)
	binary.BigEndian.PutUint16(entry[26:28], 16)
	entry = slices.Concat(entry, mediatest.Box("avcC", mediatest.AVCConfig(16, 16)))
	stsd := make([]byte, 8, 16+len(entry))
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = append(stsd, mediatest.Box("avc1", entry)...)
	stts := make([]byte, 16)
	binary.BigEndian.PutUint32(stts[4:8], 1)
	binary.BigEndian.PutUint32(stts[8:12], uint32(count))
	binary.BigEndian.PutUint32(stts[12:16], uint32(durationMS/int64(count)))
	stsc := make([]byte, 20)
	binary.BigEndian.PutUint32(stsc[4:8], 1)
	binary.BigEndian.PutUint32(stsc[8:12], 1)
	binary.BigEndian.PutUint32(stsc[12:16], uint32(count))
	binary.BigEndian.PutUint32(stsc[16:20], 1)
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[4:8], uint32(sampleSize))
	binary.BigEndian.PutUint32(stsz[8:12], uint32(count))
	stco := make([]byte, 12)
	binary.BigEndian.PutUint32(stco[4:8], 1)
	stbl := mediatest.Box("stbl", slices.Concat(
		mediatest.Box("stsd", stsd), mediatest.Box("stts", stts), mediatest.Box("stsc", stsc),
		mediatest.Box("stsz", stsz), mediatest.Box("stco", stco),
	))
	handler := make([]byte, 12)
	copy(handler[8:12], "vide")
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 1_000)
	binary.BigEndian.PutUint32(mdhd[16:20], uint32(durationMS))
	mdia := mediatest.Box("mdia", slices.Concat(mediatest.Box("mdhd", mdhd), mediatest.Box("hdlr", handler), mediatest.Box("minf", stbl)))
	return mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mdia...))
}

func runtimeMP4BoxPayload(data []byte, kind string) []byte {
	base := bytes.Index(data, []byte(kind))
	if base < 4 {
		panic("missing synthetic MP4 box " + kind)
	}
	size := int(binary.BigEndian.Uint32(data[base-4 : base]))
	return data[base+4 : base-4+size]
}

type runtimeFilesLifecycle struct {
	t         *testing.T
	data      []byte
	mediaType string
	requests  atomic.Int32
	deleted   atomic.Bool
	bodies    strings.Builder
}

func (lifecycle *runtimeFilesLifecycle) RoundTrip(request *http.Request) (*http.Response, error) {
	lifecycle.t.Helper()
	body := []byte(nil)
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(request.Body)
		require.NoError(lifecycle.t, err)
	}
	lifecycle.bodies.Write(body)
	step := lifecycle.requests.Add(1)
	fileJSON := lifecycle.fileJSON
	switch step {
	case 1:
		response := runtimeJSONResponse(request, nil)
		response.Header.Set("X-Goog-Upload-Url", "https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=synthetic-session&upload_protocol=resumable")
		return response, nil
	case 2:
		require.Equal(lifecycle.t, lifecycle.data, body)
		response := runtimeJSONResponse(request, []byte(`{"file":`+fileJSON("PROCESSING")+`}`))
		response.Header.Set("X-Goog-Upload-Status", "final")
		return response, nil
	case 3:
		return runtimeJSONResponse(request, []byte(fileJSON("PROCESSING"))), nil
	case 4:
		return runtimeJSONResponse(request, []byte(fileJSON("ACTIVE"))), nil
	case 5:
		return runtimeJSONResponse(request, runtimeVectorResponse()), nil
	case 6:
		require.Equal(lifecycle.t, http.MethodDelete, request.Method)
		lifecycle.deleted.Store(true)
		return runtimeJSONResponse(request, []byte(`{}`)), nil
	default:
		return nil, fmt.Errorf("unexpected synthetic Files request %d", step)
	}
}

func (lifecycle *runtimeFilesLifecycle) fileJSON(state string) string {
	digest := sha256.Sum256(lifecycle.data)
	created := time.Now().UTC().Truncate(time.Second)
	return `{"name":"files/file-123","displayName":"","mimeType":"` + lifecycle.mediaType +
		`","sizeBytes":"` + strconv.Itoa(len(lifecycle.data)) + `","createTime":"` + created.Format(time.RFC3339Nano) +
		`","updateTime":"` + created.Format(time.RFC3339Nano) + `","expirationTime":"` + created.Add(48*time.Hour).Format(time.RFC3339Nano) +
		`","sha256Hash":"` + base64.StdEncoding.EncodeToString(digest[:]) +
		`","uri":"https://generativelanguage.googleapis.com/v1beta/files/file-123","downloadUri":"","state":"` + state + `","source":"UPLOADED"}`
}

func runtimeJSONResponse(request *http.Request, body []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(bytes.NewReader(body)), Request: request}
}

func runtimeVectorResponse() []byte {
	values := make([]float32, 128)
	values[0] = 1
	encoded, err := json.Marshal(struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
	}{Embedding: struct {
		Values []float32 `json:"values"`
	}{Values: values}})
	if err != nil {
		panic(err)
	}
	return encoded
}
