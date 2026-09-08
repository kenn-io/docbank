package geminiembed

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	testCapabilityProfile = "1111111111111111111111111111111111111111111111111111111111111111"
	testDisclosurePolicy  = "2222222222222222222222222222222222222222222222222222222222222222"
)

func TestProfileBindsFixedContractSemanticsAndTransport(t *testing.T) {
	for _, transport := range []Transport{TransportInline, TransportFilesAPI} {
		t.Run(string(transport), func(t *testing.T) {
			profile := geminiTestProfile(t, 768)
			profile.Transport = transport
			profile.Descriptor = geminiDescriptorFor(t, profile)

			client, err := New(profile, syntheticSecrets{"secret:gemini": "synthetic-key"},
				syntheticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
			require.NoError(t, err)
			assert.Equal(t, Model, client.Descriptor().Model)
			assert.Equal(t, "epoch-1", client.Descriptor().ModelRevision)
			assert.Equal(t, 768, client.Descriptor().Dimension)
			assert.Equal(t, "title: none | text: {{content}}", client.Descriptor().ModelInput.Document.Template)
			assert.Equal(t, "task: search result | query: {{content}}", client.Descriptor().ModelInput.Query.Template)
			assert.Equal(t, Semantics{
				Contract: "gemini-embedding-2/developer-api-semantics/v1", PDFOCR: "always_enabled",
				OverlongInput: "provider_may_truncate", VideoAudio: "ignored", MaxInputTokens: 8192,
				MaxPDFPages: 6, MaxImagesPerRequest: 6, MaxAudioDurationMS: 180000,
				MaxVideoDurationMS: 120000, MaxSampledVideoFrames: 32,
			}, client.Semantics())
		})
	}
}

func TestCompatibilityEpochChangesIdentitiesButNotWireModel(t *testing.T) {
	first := geminiTestProfile(t, 128)
	second := first
	second.CompatibilityEpoch = "epoch-2"
	second.Descriptor.ModelRevision = "epoch-2"

	firstPolicy, err := PolicyFingerprint(first)
	require.NoError(t, err)
	secondPolicy, err := PolicyFingerprint(second)
	require.NoError(t, err)
	assert.NotEqual(t, firstPolicy, secondPolicy)

	first.Descriptor = geminiDescriptorFor(t, first)
	second.Descriptor = geminiDescriptorFor(t, second)
	assert.NotEqual(t, first.Descriptor.Fingerprint, second.Descriptor.Fingerprint)
	assert.Equal(t, Model, first.Descriptor.Model)
	assert.Equal(t, Model, second.Descriptor.Model)

	var requestBody string
	client := newGeminiTestClient(t, second, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, readErr := io.ReadAll(request.Body)
		require.NoError(t, readErr)
		requestBody = string(body)
		return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
	}))
	_, err = client.Embed(t.Context(), oneGeminiInput(), geminiAuthorization(second.Descriptor, 1))
	require.NoError(t, err)
	assert.Contains(t, requestBody, `"model":"models/gemini-embedding-2"`)
	assert.NotContains(t, requestBody, "epoch-2")
}

func TestProfileRejectsEpochMismatchAndMalformedBounds(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor.ModelRevision = "epoch-2"
	_, err := PolicyFingerprint(profile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compatibility epoch")

	profile = geminiTestProfile(t, 128)
	profile.PollInterval = time.Nanosecond
	_, err = PolicyFingerprint(profile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "execution bounds")
}

func TestProfileRejectsFilesAPIRequestBoundBelowWireEnvelope(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.MaxRequestBytes = int64(len(`{"file":{}}`))
	_, err := PolicyFingerprint(profile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request")
}

func TestProfileSnapshotsDescriptorAndEgressSlices(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	client, err := New(profile, syntheticSecrets{"secret:gemini": "synthetic-key"},
		syntheticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)

	profile.Descriptor.InputKinds[0] = document.EmbeddingInputRenditionChunk
	profile.EgressPolicy.AllowedCIDRs[0] = netip.MustParsePrefix("198.51.100.0/24")
	descriptor := client.Descriptor()
	assert.Equal(t, document.EmbeddingInputOriginalFile, descriptor.InputKinds[0])
	assert.Equal(t, "192.0.2.0/24", client.profile.EgressPolicy.AllowedCIDRs[0].String())

	descriptor.InputKinds[0] = document.EmbeddingInputRenditionChunk
	assert.Equal(t, document.EmbeddingInputOriginalFile, client.Descriptor().InputKinds[0])
}

func geminiTestProfile(t *testing.T, dimension int) Profile {
	t.Helper()
	epoch := "epoch-1"
	contract, err := modelInputContract()
	require.NoError(t, err)
	profile := Profile{
		CompatibilityEpoch: epoch, SecretBinding: "secret:gemini", Transport: TransportInline,
		CapabilityProfileFingerprint: testCapabilityProfile, DisclosureFingerprint: testDisclosurePolicy,
		RequestTimeout: time.Second, PollInterval: 50 * time.Millisecond, MaxPollAttempts: 3,
		CleanupTimeout: time.Second, MaxInputBytes: 4096, MaxRequestBytes: 8192, MaxResponseBytes: 16384,
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "https", Host: "generativelanguage.googleapis.com", Port: 443,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, ProxyMode: providerhttp.ProxyDisabled,
			ConnectTimeout: time.Second, KeepAlive: time.Second, TLSHandshakeTimeout: time.Second,
		},
		Descriptor: document.EmbeddingDescriptor{
			ID: ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			TrustBoundary: document.EmbeddingTrustHostedProvider, Model: Model, ModelRevision: epoch,
			Dimension: dimension, Metric: document.VectorMetricCosine,
			Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: ScalarEncodingFloat32,
			DocumentFormatter: DocumentFormatterV1, QueryFormatter: QueryFormatterV1,
			InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk},
			CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
		},
	}
	return profile
}

func geminiDescriptorFor(t *testing.T, profile Profile) document.EmbeddingDescriptor {
	t.Helper()
	fingerprint, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = fingerprint
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	return descriptor
}

type syntheticSecrets map[string]string

func (secrets syntheticSecrets) ResolveSecret(_ context.Context, binding string) (string, error) {
	return secrets[binding], nil
}

type syntheticResolver []netip.Addr

func (resolver syntheticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), resolver...), nil
}
