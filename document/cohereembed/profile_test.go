package cohereembed

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/providerhttp"
)

func TestNewRequiresExactHostedEmbedV4Profile(t *testing.T) {
	for _, dimension := range []int{256, 512, 1024, 1536} {
		profile := testProfile(t, dimension)
		client, err := New(profile, testSecrets{"secret:cohere": "synthetic-key"},
			testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
		require.NoError(t, err)
		assert.Equal(t, profile.Descriptor, client.Descriptor())
	}

	profile := testProfile(t, 1024)
	mutations := map[string]func(*Profile){
		"model":     func(value *Profile) { value.Descriptor.Model = "embed-v3.0" },
		"dimension": func(value *Profile) { value.Descriptor.Dimension = 768 },
		"epoch":     func(value *Profile) { value.CompatibilityEpoch = "other" },
		"input kind": func(value *Profile) {
			value.Descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}
		},
		"normalization": func(value *Profile) { value.Descriptor.Normalization = document.VectorNormalizationUnitLength },
		"secret":        func(value *Profile) { value.SecretBinding = "" },
		"host":          func(value *Profile) { value.EgressPolicy.Host = "example.com" },
		"images":        func(value *Profile) { value.MaxImageBytes = (20 << 20) + 1 },
		"batch":         func(value *Profile) { value.MaxBatchItems = 97 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := profile
			mutate(&changed)
			_, err := PolicyFingerprint(changed)
			require.Error(t, err)
		})
	}
}

func TestPolicyFingerprintRejectsDuplicateEgressAuthorityWithoutMutatingProfile(t *testing.T) {
	profile := testProfile(t, 1024)
	prefix := profile.EgressPolicy.AllowedCIDRs[0]
	profile.EgressPolicy.AllowedCIDRs = []netip.Prefix{prefix, prefix}
	original := append([]netip.Prefix(nil), profile.EgressPolicy.AllowedCIDRs...)

	_, err := PolicyFingerprint(profile)
	require.Error(t, err)
	assert.Equal(t, original, profile.EgressPolicy.AllowedCIDRs)
}

func TestPolicyFingerprintBindsProviderBoundsAndDoesNotMutateCaller(t *testing.T) {
	profile := testProfile(t, 1024)
	originalCIDRs := slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	base, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	mutations := map[string]func(*Profile){
		"dimension": func(value *Profile) { value.Descriptor.Dimension = 1536 },
		"epoch": func(value *Profile) {
			value.CompatibilityEpoch = "deployment-2026-09"
			value.Descriptor.ModelRevision = value.CompatibilityEpoch
		},
		"binding": func(value *Profile) { value.SecretBinding = "secret:other" },
		"item":    func(value *Profile) { value.MaxInputItemBytes-- },
		"input":   func(value *Profile) { value.MaxInputBytes-- },
		"image": func(value *Profile) {
			value.MaxImageBytes--
			value.MediaPolicy.MaxBytes--
		},
		"request":  func(value *Profile) { value.MaxRequestBytes-- },
		"response": func(value *Profile) { value.MaxResponseBytes-- },
		"timeout":  func(value *Profile) { value.RequestTimeout += time.Second },
		"media":    func(value *Profile) { value.MediaPolicy.MaxPixels-- },
		"egress": func(value *Profile) {
			value.EgressPolicy.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := profile
			mutate(&changed)
			fingerprint, fingerprintErr := PolicyFingerprint(changed)
			require.NoError(t, fingerprintErr)
			assert.NotEqual(t, base, fingerprint)
		})
	}
	assert.Equal(t, originalCIDRs, profile.EgressPolicy.AllowedCIDRs)
}

func TestNewReplacesCallerHTTPAuthority(t *testing.T) {
	profile := testProfile(t, 1024)
	ambient := &countingTransport{}
	supplied := &http.Client{Transport: ambient, Jar: testJar{}, Timeout: time.Hour,
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	client, err := New(profile, testSecrets{"secret:cohere": "synthetic-key"},
		testResolver{netip.MustParseAddr("192.0.2.10")}, supplied)
	require.NoError(t, err)
	assert.NotSame(t, supplied, client.http)
	assert.NotSame(t, ambient, client.http.Transport)
	assert.Nil(t, client.http.Jar)
	assert.Zero(t, client.http.Timeout)
	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	require.NoError(t, err)
	require.ErrorIs(t, client.http.CheckRedirect(request, nil), http.ErrUseLastResponse)
}

func testProfile(t *testing.T, dimension int) Profile {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "cohere/embed-v4/search/v1",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeDocument, Template: "{{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeQuery, Template: "{{content}}"},
	})
	require.NoError(t, err)
	profile := Profile{
		Descriptor: document.EmbeddingDescriptor{
			ID: ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			TrustBoundary: document.EmbeddingTrustHostedProvider, Model: Model,
			ModelRevision: "deployment-2026-08", Dimension: dimension, Metric: document.VectorMetricCosine,
			Normalization: document.VectorNormalizationNone, ScalarEncoding: ScalarEncodingFloat32,
			DocumentFormatter: DocumentFormatterV1, QueryFormatter: QueryFormatterV1,
			InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk},
			CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery},
		},
		CompatibilityEpoch: "deployment-2026-08", SecretBinding: "secret:cohere",
		RequestTimeout: time.Second, MaxBatchItems: 96, MaxInputItemBytes: 1 << 20,
		MaxInputBytes: 20 << 20, MaxImageBytes: 20 << 20, MaxRequestBytes: 32 << 20,
		MaxResponseBytes: 32 << 20,
		MediaPolicy: media.Policy{MaxBytes: 20 << 20, MaxPixels: media.DefaultMaxPixels,
			MaxFrames: media.DefaultMaxFrames, AllowStill: true, AllowAnimated: true},
		EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.cohere.com", Port: 443,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
			ProxyMode:    providerhttp.ProxyDisabled, ConnectTimeout: time.Second,
			KeepAlive: time.Second, TLSHandshakeTimeout: time.Second},
	}
	profile.Descriptor = descriptorFor(t, profile)
	return profile
}

func descriptorFor(t *testing.T, profile Profile) document.EmbeddingDescriptor {
	t.Helper()
	profile.Descriptor.PolicyFingerprint = ""
	profile.Descriptor.Fingerprint = ""
	fingerprint, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = fingerprint
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	return descriptor
}

type testSecrets map[string]string

func (secrets testSecrets) ResolveSecret(_ context.Context, binding string) (string, error) {
	value, ok := secrets[binding]
	if !ok {
		return "", errors.New("missing synthetic secret")
	}
	return value, nil
}

type testResolver []netip.Addr

func (resolver testResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), resolver...), nil
}

type countingTransport struct{ calls int }

func (transport *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls++
	return nil, errors.New("ambient transport must not run")
}

type testJar struct{}

func (testJar) SetCookies(*url.URL, []*http.Cookie) {}
func (testJar) Cookies(*url.URL) []*http.Cookie     { return nil }
