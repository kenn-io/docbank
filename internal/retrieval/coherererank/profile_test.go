package coherererank

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/providerhttp"
)

func TestNewRequiresIndependentFixedRerankV4Profile(t *testing.T) {
	for _, model := range []Model{ModelPro, ModelFast} {
		profile := testProfile(model)
		client, err := New(profile, testSecrets{"secret:cohere-rerank": "synthetic-key"},
			testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
		require.NoError(t, err)
		assert.Equal(t, profile.ID, client.ProfileID())
		assert.Equal(t, model, client.Model())
	}

	mutations := map[string]func(*Profile){
		"id":         func(value *Profile) { value.ID = "" },
		"model":      func(value *Profile) { value.Model = "rerank-v3.5" },
		"epoch":      func(value *Profile) { value.CompatibilityEpoch = "other" },
		"binding":    func(value *Profile) { value.SecretBinding = "" },
		"candidates": func(value *Profile) { value.MaxCandidates = 1001 },
		"tokens":     func(value *Profile) { value.MaxTokensPerDocument = value.MaxExcerptBytes - 1 },
		"host":       func(value *Profile) { value.EgressPolicy.Host = "example.com" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			profile := testProfile(ModelPro)
			mutate(&profile)
			_, err := PolicyFingerprint(profile)
			require.Error(t, err)
		})
	}
}

func TestNewAppliesConservativeRerankDefaults(t *testing.T) {
	profile := testProfile(ModelPro)
	profile.RequestTimeout = 0
	profile.MaxCandidates = 0
	profile.MaxQueryBytes = 0
	profile.MaxExcerptBytes = 0
	profile.MaxTotalExcerptBytes = 0
	profile.MaxRequestBytes = 0
	profile.MaxResponseBytes = 0
	profile.MaxTokensPerDocument = 0

	client, err := New(profile, testSecrets{"secret:cohere-rerank": "synthetic-key"},
		testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, client.profile.RequestTimeout)
	assert.Equal(t, 100, client.profile.MaxCandidates)
	assert.Equal(t, 4096, client.profile.MaxQueryBytes)
	assert.Equal(t, 4096, client.profile.MaxExcerptBytes)
	assert.Equal(t, int64(4<<20), client.profile.MaxTotalExcerptBytes)
	assert.Equal(t, int64(8<<20), client.profile.MaxRequestBytes)
	assert.Equal(t, int64(8<<20), client.profile.MaxResponseBytes)
	assert.Equal(t, 4096, client.profile.MaxTokensPerDocument)
}

func TestPolicyFingerprintBindsIndependentRerankAuthorityWithoutMutatingProfile(t *testing.T) {
	profile := testProfile(ModelPro)
	profile.EgressPolicy.AllowedCIDRs = []netip.Prefix{
		netip.MustParsePrefix("198.51.100.99/24"),
		netip.MustParsePrefix("192.0.2.99/24"),
	}
	profile.EgressPolicy.TLS.SPKISHA256 = []string{strings.Repeat("B", 64), strings.Repeat("A", 64)}
	originalCIDRs := slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	originalPins := slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
	base, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	mutations := map[string]func(*Profile){
		"model":      func(value *Profile) { value.Model = ModelFast },
		"epoch":      func(value *Profile) { value.CompatibilityEpoch, value.ModelRevision = "next", "next" },
		"binding":    func(value *Profile) { value.SecretBinding = "secret:other" },
		"timeout":    func(value *Profile) { value.RequestTimeout += time.Second },
		"candidates": func(value *Profile) { value.MaxCandidates-- },
		"query":      func(value *Profile) { value.MaxQueryBytes-- },
		"excerpt":    func(value *Profile) { value.MaxExcerptBytes-- },
		"total":      func(value *Profile) { value.MaxTotalExcerptBytes-- },
		"request":    func(value *Profile) { value.MaxRequestBytes-- },
		"response":   func(value *Profile) { value.MaxResponseBytes-- },
		"tokens": func(value *Profile) {
			value.MaxExcerptBytes--
			value.MaxTokensPerDocument--
		},
		"egress": func(value *Profile) {
			value.EgressPolicy.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
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
	assert.Equal(t, originalPins, profile.EgressPolicy.TLS.SPKISHA256)

	_, err = New(profile, testSecrets{"secret:cohere-rerank": "synthetic-key"},
		testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)
	assert.Equal(t, originalCIDRs, profile.EgressPolicy.AllowedCIDRs)
	assert.Equal(t, originalPins, profile.EgressPolicy.TLS.SPKISHA256)
}

func TestNewReplacesCallerHTTPAuthority(t *testing.T) {
	ambient := &countingTransport{}
	supplied := &http.Client{Transport: ambient, Jar: testJar{}, Timeout: time.Hour,
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	client, err := New(testProfile(ModelPro), testSecrets{"secret:cohere-rerank": "synthetic-key"},
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

func testProfile(model Model) Profile {
	return Profile{ID: "cohere-rerank", Model: model, CompatibilityEpoch: "deployment-2026-08",
		ModelRevision: "deployment-2026-08", SecretBinding: "secret:cohere-rerank",
		RequestTimeout: time.Second, MaxCandidates: 1000, MaxQueryBytes: 4096,
		MaxExcerptBytes: 4096, MaxTotalExcerptBytes: 4 << 20, MaxRequestBytes: 8 << 20,
		MaxResponseBytes: 8 << 20, MaxTokensPerDocument: 4096,
		EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.cohere.com", Port: 443,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
			ProxyMode:    providerhttp.ProxyDisabled, ConnectTimeout: time.Second,
			KeepAlive: time.Second, TLSHandshakeTimeout: time.Second}}
}

type testSecrets map[string]string

func (secrets testSecrets) ResolveSecret(_ context.Context, binding string) (string, error) {
	value, ok := secrets[binding]
	if !ok {
		return "", errors.New("missing synthetic secret")
	}
	return value, nil
}

type secretResolverFunc func(context.Context, string) (string, error)

func (resolver secretResolverFunc) ResolveSecret(ctx context.Context, binding string) (string, error) {
	return resolver(ctx, binding)
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
