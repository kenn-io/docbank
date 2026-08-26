package openaihosted

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

func TestNewRequiresTheFixedHostedDescriptorEpochAndCredential(t *testing.T) {
	profile := hostedTestProfile(t)
	resolver := hostedSecrets{"secret:openai": "sk-synthetic"}

	client, err := New(profile, resolver, staticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)
	assert.Equal(t, profile.Descriptor, client.Descriptor())

	tests := map[string]func(*Profile){
		"provider id":        func(value *Profile) { value.Descriptor.ID = "openai-compatible.embeddings-v1" },
		"contract version":   func(value *Profile) { value.Descriptor.ContractVersion++ },
		"hosted trust":       func(value *Profile) { value.Descriptor.TrustBoundary = document.EmbeddingTrustOperatorNetwork },
		"fixed model":        func(value *Profile) { value.Descriptor.Model = "text-embedding-3-small" },
		"positive dimension": func(value *Profile) { value.Descriptor.Dimension = 0 },
		"metric":             func(value *Profile) { value.Descriptor.Metric = document.VectorMetricL2 },
		"normalization":      func(value *Profile) { value.Descriptor.Normalization = document.VectorNormalizationNone },
		"scalar":             func(value *Profile) { value.Descriptor.ScalarEncoding = "float64" },
		"document formatter": func(value *Profile) { value.Descriptor.DocumentFormatter = "custom" },
		"query formatter":    func(value *Profile) { value.Descriptor.QueryFormatter = "custom" },
		"input kinds": func(value *Profile) {
			value.Descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
		},
		"request modes": func(value *Profile) {
			value.Descriptor.SupportedRequestModes = []document.ModelInputMode{document.ModelInputModeDocument}
		},
		"query support":      func(value *Profile) { value.Descriptor.SupportsTextQuery = false },
		"compatibility":      func(value *Profile) { value.Descriptor.CompatibilityID = "forged-space" },
		"revision epoch":     func(value *Profile) { value.Descriptor.ModelRevision = "different-epoch" },
		"missing epoch":      func(value *Profile) { value.CompatibilityEpoch = "" },
		"missing binding":    func(value *Profile) { value.SecretBinding = "" },
		"whitespace binding": func(value *Profile) { value.SecretBinding = "secret: openai" },
		"control binding":    func(value *Profile) { value.SecretBinding = "secret:openai\n" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := hostedTestProfile(t)
			changed.Descriptor.Fingerprint = ""
			mutate(&changed)
			if changed.Descriptor.Dimension > 0 {
				changed.Descriptor = canonicalDescriptorWithoutPolicyCheck(t, changed.Descriptor)
			}
			_, err := New(changed, resolver, staticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
			require.Error(t, err)
		})
	}

	_, err = New(profile, nil, staticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.Error(t, err, "a named API-key binding always requires a resolver")
	_, err = New(profile, resolver, staticResolver{netip.MustParseAddr("192.0.2.10")}, nil)
	require.Error(t, err, "the harmless client settings source is explicit")
}

func TestHostedBatchLimitAccepts2048AndRejects2049BeforeRequestAuthority(t *testing.T) {
	accepted := hostedTestProfile(t)
	accepted.MaxBatchItems = 2048
	accepted.Descriptor = hostedDescriptorFor(t, accepted)

	_, err := New(accepted, hostedSecrets{"secret:openai": "sk-synthetic"}, staticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)

	rejected := accepted
	rejected.MaxBatchItems = 2049
	_, _, err = normalizeProfile(rejected)
	require.Error(t, err)
}

func TestPolicyFingerprintCoversFixedContractBoundsAndExactEgressWithoutMutatingCaller(t *testing.T) {
	profile := hostedTestProfile(t)
	originalCIDRs := slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	originalPins := slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
	base, err := PolicyFingerprint(profile)
	require.NoError(t, err)

	mutations := map[string]func(*Profile){
		"epoch": func(value *Profile) {
			value.CompatibilityEpoch = "hosted-epoch-2"
			value.Descriptor.ModelRevision = "hosted-epoch-2"
		},
		"secret binding": func(value *Profile) { value.SecretBinding = "secret:other" },
		"dimension":      func(value *Profile) { value.Descriptor.Dimension++ },
		"model input": func(value *Profile) {
			contract, contractErr := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
			require.NoError(t, contractErr)
			value.Descriptor.ModelInput = contract
			value.Descriptor.CompatibilityID = contract.CompatibilityID
		},
		"batch":       func(value *Profile) { value.MaxBatchItems++ },
		"per item":    func(value *Profile) { value.MaxInputItemBytes++ },
		"total input": func(value *Profile) { value.MaxInputBytes++ },
		"request":     func(value *Profile) { value.MaxRequestBytes++ },
		"response":    func(value *Profile) { value.MaxResponseBytes++ },
		"timeout":     func(value *Profile) { value.RequestTimeout += time.Second },
		"CIDR": func(value *Profile) {
			value.EgressPolicy.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
		},
		"SPKI": func(value *Profile) {
			value.EgressPolicy.TLS.SPKISHA256 = []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := hostedTestProfile(t)
			mutate(&changed)
			fingerprint, fingerprintErr := PolicyFingerprint(changed)
			require.NoError(t, fingerprintErr)
			assert.NotEqual(t, base, fingerprint)
		})
	}
	assert.Equal(t, originalCIDRs, profile.EgressPolicy.AllowedCIDRs)
	assert.Equal(t, originalPins, profile.EgressPolicy.TLS.SPKISHA256)

	invalid := hostedTestProfile(t)
	invalid.EgressPolicy.Host = "example.com"
	_, err = PolicyFingerprint(invalid)
	require.Error(t, err)
	invalid = hostedTestProfile(t)
	invalid.EgressPolicy.Port = 8443
	_, err = PolicyFingerprint(invalid)
	require.Error(t, err)
	invalid = hostedTestProfile(t)
	invalid.EgressPolicy.TLS.RootCAs = x509.NewCertPool()
	_, err = PolicyFingerprint(invalid)
	require.Error(t, err, "custom roots expand hosted trust authority")
}

func TestNewReplacesCallerTransportJarRedirectAndTimeoutAuthority(t *testing.T) {
	profile := hostedTestProfile(t)
	originalCIDRs := slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	originalPins := slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
	ambient := &countingTransport{}
	client, err := New(profile, hostedSecrets{"secret:openai": "sk-synthetic"},
		staticResolver{netip.MustParseAddr("203.0.113.7")},
		&http.Client{Transport: ambient, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }, Timeout: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, originalCIDRs, profile.EgressPolicy.AllowedCIDRs)
	assert.Equal(t, originalPins, profile.EgressPolicy.TLS.SPKISHA256)
	assert.NotSame(t, ambient, client.http.Transport)
	assert.Zero(t, client.http.Timeout)
	assert.Nil(t, client.http.Jar)
	require.NotNil(t, client.http.CheckRedirect)
	require.ErrorIs(t, client.http.CheckRedirect(new(http.Request), nil), http.ErrUseLastResponse)
	_, err = client.Embed(context.Background(), oneHostedInput(), hostedAuthorization(client.descriptor, 1))
	require.ErrorIs(t, err, ErrTransientResponse)
	assert.Zero(t, ambient.calls, "the caller transport must never receive hosted authority")
}

func TestEmbedSendsExactHostedRequestAndRestoresReturnedIndices(t *testing.T) {
	profile := hostedTestProfile(t)
	inputs := []document.EmbeddingInput{
		{Key: "document-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha"},
		{Key: "query-a", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "beta"},
	}
	originalInputs := slices.Clone(inputs)
	var calls int
	client := newHostedTestClient(t, profile, hostedSecrets{"secret:openai": "sk-synthetic"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "https://api.openai.com/v1/embeddings", request.URL.String())
		assert.Equal(t, "api.openai.com", request.URL.Host)
		assert.Equal(t, "application/json", request.Header.Get("Accept"))
		assert.Equal(t, "application/json", request.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer sk-synthetic", request.Header.Get("Authorization"))
		assert.Empty(t, request.Header.Get("Cookie"))
		payload, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"input":["passage: alpha","query: beta"],
			"model":"text-embedding-3-large",
			"dimensions":3,
			"encoding_format":"float"
		}`, string(payload))
		return hostedJSONResponse(request, http.StatusOK, `{
			"object":"list",
			"data":[
				{"object":"embedding","embedding":[0,1,0],"index":1},
				{"object":"embedding","embedding":[1,0,0],"index":0}
			],
			"model":"text-embedding-3-large",
			"usage":{"prompt_tokens":2,"total_tokens":2}
		}`), nil
	}))

	result, err := client.Embed(context.Background(), inputs, hostedAuthorization(profile.Descriptor, 2))
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, originalInputs, inputs, "embedding must not mutate caller slices")
	assert.Equal(t, document.EmbeddingResult{Vectors: []document.EmbeddingVector{
		{Key: "document-a", Values: []float32{1, 0, 0}},
		{Key: "query-a", Values: []float32{0, 1, 0}},
	}}, result)
}

func hostedTestProfile(t *testing.T) Profile {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileE5})
	require.NoError(t, err)
	profile := Profile{
		Descriptor: document.EmbeddingDescriptor{
			ID: ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			TrustBoundary: document.EmbeddingTrustHostedProvider, Model: Model,
			ModelRevision: "hosted-epoch-1", Dimension: 3, Metric: document.VectorMetricCosine,
			Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: ScalarEncodingFloat32,
			DocumentFormatter: DocumentFormatterV1, QueryFormatter: QueryFormatterV1,
			InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk},
			CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
		},
		CompatibilityEpoch: "hosted-epoch-1", SecretBinding: "secret:openai",
		RequestTimeout: time.Second, MaxBatchItems: 8, MaxInputItemBytes: 2048,
		MaxInputBytes: 4096, MaxRequestBytes: 8192, MaxResponseBytes: 16384,
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "https", Host: "api.openai.com", Port: 443,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("192.0.2.0/24")},
			ProxyMode:    providerhttp.ProxyDisabled, ConnectTimeout: time.Second,
			KeepAlive: time.Second, TLSHandshakeTimeout: time.Second,
			TLS: providerhttp.TLSPolicy{SPKISHA256: []string{
				"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
		},
	}
	profile.Descriptor = hostedDescriptorFor(t, profile)
	return profile
}

func hostedDescriptorFor(t *testing.T, profile Profile) document.EmbeddingDescriptor {
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

func canonicalDescriptorWithoutPolicyCheck(t *testing.T, value document.EmbeddingDescriptor) document.EmbeddingDescriptor {
	t.Helper()
	canonical, err := document.NewEmbeddingDescriptor(value)
	if err != nil {
		return value
	}
	return canonical
}

type hostedSecrets map[string]string

func (secrets hostedSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	value, ok := secrets[name]
	if !ok {
		return "", errors.New("synthetic secret missing")
	}
	return value, nil
}

type staticResolver []netip.Addr

func (resolver staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return slices.Clone(resolver), nil
}

type countingTransport struct{ calls int }

func (transport *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls++
	return nil, errors.New("ambient transport must not run")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func newHostedTestClient(t *testing.T, profile Profile, secrets SecretResolver, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := New(profile, secrets, staticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)
	client.http.Transport = transport
	return client
}

func hostedAuthorization(descriptor document.EmbeddingDescriptor, batch int) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: batch,
		MaxInputBytes: 4096, MaxResponseBytes: 4096,
	}
}

func hostedJSONResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(body)), Request: request,
	}
}
