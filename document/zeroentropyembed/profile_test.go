package zeroentropyembed

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

func TestNewRequiresFixedZembedProfile(t *testing.T) {
	for _, dimension := range []int{2560, 1280, 640, 320, 160, 80, 40} {
		for _, encoding := range []EncodingFormat{EncodingFloat, EncodingBase64} {
			profile := testProfile(t, dimension, encoding, LatencyAuto)
			client, err := New(profile, testSecrets{"secret:zeroentropy": "synthetic-key"},
				testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
			require.NoError(t, err)
			assert.Equal(t, profile.Descriptor, client.Descriptor())
		}
	}

	mutations := map[string]func(*Profile){
		"model":     func(value *Profile) { value.Descriptor.Model = "zembed-2" },
		"dimension": func(value *Profile) { value.Descriptor.Dimension = 768 },
		"epoch":     func(value *Profile) { value.CompatibilityEpoch = "other" },
		"encoding":  func(value *Profile) { value.EncodingFormat = "json" },
		"latency":   func(value *Profile) { value.Latency = "instant" },
		"transform": func(value *Profile) { value.ClientTransform = "truncate" },
		"secret":    func(value *Profile) { value.SecretBinding = "" },
		"host":      func(value *Profile) { value.EgressPolicy.Host = "example.com" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			profile := testProfile(t, 640, EncodingBase64, LatencyFast)
			mutate(&profile)
			_, err := PolicyFingerprint(profile)
			require.Error(t, err)
		})
	}
}

func TestPolicyFingerprintBindsZembedExecutionIdentityWithoutMutatingProfile(t *testing.T) {
	profile := testProfile(t, 640, EncodingBase64, LatencyFast)
	original := slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	base, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	mutations := map[string]func(*Profile){
		"dimension": func(value *Profile) { value.Descriptor.Dimension = 320 },
		"epoch": func(value *Profile) {
			value.CompatibilityEpoch, value.Descriptor.ModelRevision = "deployment-2026-09", "deployment-2026-09"
		},
		"encoding": func(value *Profile) { value.EncodingFormat = EncodingFloat },
		"latency":  func(value *Profile) { value.Latency = LatencySlow },
		"binding":  func(value *Profile) { value.SecretBinding = "secret:other" },
		"batch":    func(value *Profile) { value.MaxBatchItems-- },
		"item":     func(value *Profile) { value.MaxInputItemBytes-- },
		"input":    func(value *Profile) { value.MaxInputBytes-- },
		"request":  func(value *Profile) { value.MaxRequestBytes-- },
		"response": func(value *Profile) { value.MaxResponseBytes-- },
		"timeout":  func(value *Profile) { value.RequestTimeout += time.Second },
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
	assert.Equal(t, original, profile.EgressPolicy.AllowedCIDRs)
}

func TestNewAppliesDefaultAndMaximumExecutionBounds(t *testing.T) {
	defaults := testProfile(t, 40, EncodingFloat, LatencyFast)
	defaults.EncodingFormat = ""
	defaults.Latency = ""
	defaults.ClientTransform = ""
	defaults.RequestTimeout = 0
	defaults.MaxBatchItems = 0
	defaults.MaxInputItemBytes = 0
	defaults.MaxInputBytes = 0
	defaults.MaxRequestBytes = 0
	defaults.MaxResponseBytes = 0
	defaults.Descriptor = descriptorFor(t, defaults)
	client, err := New(defaults, testSecrets{"secret:zeroentropy": "synthetic-key"},
		testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)
	assert.Equal(t, EncodingFloat, client.profile.EncodingFormat)
	assert.Equal(t, LatencyAuto, client.profile.Latency)
	assert.Equal(t, TransformNone, client.profile.ClientTransform)
	assert.Equal(t, 30*time.Second, client.profile.RequestTimeout)
	assert.Equal(t, 128, client.profile.MaxBatchItems)
	assert.Equal(t, int64(1<<20), client.profile.MaxInputItemBytes)
	assert.Equal(t, int64(4<<20), client.profile.MaxInputBytes)
	assert.Equal(t, int64(8<<20), client.profile.MaxRequestBytes)
	assert.Equal(t, int64(32<<20), client.profile.MaxResponseBytes)

	maximums := testProfile(t, 40, EncodingFloat, LatencyFast)
	maximums.RequestTimeout = 5 * time.Minute
	maximums.MaxBatchItems = 2048
	maximums.MaxInputItemBytes = 5_000_000
	maximums.MaxInputBytes = 5_000_000
	maximums.MaxRequestBytes = 16 << 20
	maximums.MaxResponseBytes = 128 << 20
	maximums.Descriptor = descriptorFor(t, maximums)
	_, err = New(maximums, testSecrets{"secret:zeroentropy": "synthetic-key"},
		testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)

	for name, mutate := range map[string]func(*Profile){
		"timeout":  func(profile *Profile) { profile.RequestTimeout = 5*time.Minute + time.Nanosecond },
		"batch":    func(profile *Profile) { profile.MaxBatchItems = 2049 },
		"item":     func(profile *Profile) { profile.MaxInputItemBytes = 5_000_001 },
		"input":    func(profile *Profile) { profile.MaxInputBytes = 5_000_001 },
		"request":  func(profile *Profile) { profile.MaxRequestBytes = 16<<20 + 1 },
		"response": func(profile *Profile) { profile.MaxResponseBytes = 128<<20 + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := maximums
			mutate(&invalid)
			_, err := PolicyFingerprint(invalid)
			require.Error(t, err)
		})
	}
}

func TestNewReplacesAmbientHTTPBehaviorAndClonesMutableProfileSlices(t *testing.T) {
	profile := testProfile(t, 40, EncodingFloat, LatencyFast)
	profile.EgressPolicy.TLS.SPKISHA256 = []string{strings.Repeat("0", 64)}
	profile.Descriptor = descriptorFor(t, profile)
	suppliedTransport := &inertTransport{}
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	suppliedRedirect := func(*http.Request, []*http.Request) error { return nil }
	supplied := &http.Client{Transport: suppliedTransport, Jar: jar, CheckRedirect: suppliedRedirect, Timeout: time.Minute}

	client, err := New(profile, testSecrets{"secret:zeroentropy": "synthetic-key"},
		testResolver{netip.MustParseAddr("192.0.2.10")}, supplied)
	require.NoError(t, err)
	assert.NotSame(t, suppliedTransport, client.http.Transport)
	assert.Nil(t, client.http.Jar)
	require.ErrorIs(t, client.http.CheckRedirect(&http.Request{}, nil), http.ErrUseLastResponse)
	assert.Zero(t, client.http.Timeout)
	assert.Same(t, suppliedTransport, supplied.Transport)
	assert.Same(t, jar, supplied.Jar)
	require.NoError(t, supplied.CheckRedirect(&http.Request{}, nil))
	assert.Equal(t, time.Minute, supplied.Timeout)

	wantDescriptor := client.Descriptor()
	wantCIDRs := slices.Clone(client.profile.EgressPolicy.AllowedCIDRs)
	wantPins := slices.Clone(client.profile.EgressPolicy.TLS.SPKISHA256)
	profile.Descriptor.InputKinds[0] = document.EmbeddingInputQueryText
	profile.Descriptor.SupportedRequestModes[0] = "mutated"
	profile.EgressPolicy.AllowedCIDRs[0] = netip.MustParsePrefix("198.51.100.0/24")
	profile.EgressPolicy.TLS.SPKISHA256[0] = strings.Repeat("f", 64)
	assert.Equal(t, wantDescriptor, client.Descriptor())
	assert.Equal(t, wantCIDRs, client.profile.EgressPolicy.AllowedCIDRs)
	assert.Equal(t, wantPins, client.profile.EgressPolicy.TLS.SPKISHA256)

	returned := client.Descriptor()
	returned.InputKinds[0] = document.EmbeddingInputQueryText
	returned.SupportedRequestModes[0] = "mutated"
	assert.Equal(t, wantDescriptor, client.Descriptor())
}

func testProfile(t *testing.T, dimension int, encoding EncodingFormat, latency Latency) Profile {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "zeroentropy/zembed-1/retrieval/v1",
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
			InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk},
			CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery},
		},
		CompatibilityEpoch: "deployment-2026-08", SecretBinding: "secret:zeroentropy",
		EncodingFormat: encoding, Latency: latency, ClientTransform: TransformNone,
		RequestTimeout: time.Second, MaxBatchItems: 128, MaxInputItemBytes: 1 << 20,
		MaxInputBytes: 4 << 20, MaxRequestBytes: 8 << 20, MaxResponseBytes: 32 << 20,
		EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.zeroentropy.dev", Port: 443,
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

type inertTransport struct{}

func (*inertTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("ambient transport must not be used")
}
