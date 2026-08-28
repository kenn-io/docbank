package zeroentropyembed

import (
	"context"
	"errors"
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
