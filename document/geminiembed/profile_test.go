package geminiembed

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

// TestProfileBindsGeminiSearchPrefixesDimensionAndTransport catches a profile
// that permits a different Gemini model, role envelope, vector dimension, or
// direct-file transport identity.
func TestProfileBindsGeminiSearchPrefixesDimensionAndTransport(t *testing.T) {
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile:         document.ModelInputProfileCustom,
		CompatibilityID: "gemini-embedding-2/search/v1",
		Document: document.ModelInputEncoder{
			Mode: document.ModelInputModeText, Template: "title: none | text: {{content}}",
		},
		Query: document.ModelInputEncoder{
			Mode: document.ModelInputModeText, Template: "task: search result | query: {{content}}",
		},
	})
	require.NoError(t, err)

	for _, transport := range []Transport{TransportInline, TransportFilesAPI} {
		t.Run(string(transport), func(t *testing.T) {
			profile := Profile{
				CompatibilityEpoch:           "gemini-embedding-2",
				SecretBinding:                "secret:gemini",
				Transport:                    transport,
				CapabilityProfileFingerprint: testCapabilityProfile,
				DisclosureFingerprint:        testDisclosurePolicy,
				RequestTimeout:               time.Second,
				MaxInputBytes:                4096,
				MaxRequestBytes:              8192,
				MaxResponseBytes:             16384,
				EgressPolicy: providerhttp.EgressPolicy{
					Scheme: "https", Host: "generativelanguage.googleapis.com", Port: 443,
					AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
					ProxyMode:    providerhttp.ProxyDisabled, ConnectTimeout: time.Second,
					KeepAlive: time.Second, TLSHandshakeTimeout: time.Second,
				},
				Descriptor: document.EmbeddingDescriptor{
					ID: ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
					TrustBoundary: document.EmbeddingTrustHostedProvider, Model: "gemini-embedding-2",
					ModelRevision: "gemini-embedding-2", Dimension: 768, Metric: document.VectorMetricCosine,
					Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: ScalarEncodingFloat32,
					DocumentFormatter: DocumentFormatterV1, QueryFormatter: QueryFormatterV1,
					InputKinds: []document.EmbeddingInputKind{
						document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk,
					},
					CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
					SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
				},
			}
			fingerprint, err := PolicyFingerprint(profile)
			require.NoError(t, err)
			profile.Descriptor.PolicyFingerprint = fingerprint
			profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
			require.NoError(t, err)

			client, err := New(profile, syntheticSecrets{"secret:gemini": "synthetic-key"},
				syntheticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
			require.NoError(t, err)
			assert.Equal(t, "gemini-embedding-2", client.Descriptor().Model)
			assert.Equal(t, 768, client.Descriptor().Dimension)
			assert.Equal(t, "title: none | text: {{content}}", client.Descriptor().ModelInput.Document.Template)
			assert.Equal(t, "task: search result | query: {{content}}", client.Descriptor().ModelInput.Query.Template)
		})
	}
}

// TestProfileRejectsFilesAPIRequestBoundBelowFixedEnvelopes catches a profile
// that can encode the resumable start body but cannot encode the supported
// fileData embedding envelope it promises.
func TestProfileRejectsFilesAPIRequestBoundBelowFixedEnvelopes(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.MaxRequestBytes = int64(len(`{"file":{}}`))

	_, err := PolicyFingerprint(profile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request")
}

func TestProfileRejectsUnsafePollInterval(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.PollInterval = time.Nanosecond

	_, err := PolicyFingerprint(profile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "execution bounds")
}

func TestProfileBindsFinitePollAttemptCapacity(t *testing.T) {
	first := geminiTestProfile(t, 128)
	first.MaxPollAttempts = 3
	firstFingerprint, err := PolicyFingerprint(first)
	require.NoError(t, err)

	second := first
	second.MaxPollAttempts = 4
	secondFingerprint, err := PolicyFingerprint(second)
	require.NoError(t, err)
	assert.NotEqual(t, firstFingerprint, secondFingerprint)

	first.MaxPollAttempts = -1
	_, err = PolicyFingerprint(first)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "execution bounds")
}

type syntheticSecrets map[string]string

func (secrets syntheticSecrets) ResolveSecret(_ context.Context, binding string) (string, error) {
	return secrets[binding], nil
}

type syntheticResolver []netip.Addr

func (resolver syntheticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), resolver...), nil
}
