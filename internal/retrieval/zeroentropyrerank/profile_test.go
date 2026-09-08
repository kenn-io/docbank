package zeroentropyrerank

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
	"go.kenn.io/docbank/document/providerhttp"
)

func TestNewRequiresIndependentFixedZerankProfile(t *testing.T) {
	for _, latency := range []Latency{LatencyAuto, LatencyFast, LatencySlow} {
		profile := testProfile(latency)
		client, err := New(profile, testSecrets{"secret:zeroentropy-rerank": "synthetic-key"},
			testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
		require.NoError(t, err)
		assert.Equal(t, profile.ID, client.ProfileID())
		assert.Equal(t, Model, client.Model())
	}

	mutations := map[string]func(*Profile){
		"id":         func(value *Profile) { value.ID = "" },
		"model":      func(value *Profile) { value.Model = "zerank-1" },
		"epoch":      func(value *Profile) { value.CompatibilityEpoch = "other" },
		"latency":    func(value *Profile) { value.Latency = "instant" },
		"binding":    func(value *Profile) { value.SecretBinding = "" },
		"candidates": func(value *Profile) { value.MaxCandidates = 2049 },
		"host":       func(value *Profile) { value.EgressPolicy.Host = "example.com" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			profile := testProfile(LatencyFast)
			mutate(&profile)
			_, err := PolicyFingerprint(profile)
			require.Error(t, err)
		})
	}
}

func TestPolicyFingerprintBindsZerankAuthorityWithoutMutatingProfile(t *testing.T) {
	profile := testProfile(LatencyFast)
	original := slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	base, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	mutations := map[string]func(*Profile){
		"epoch":      func(value *Profile) { value.CompatibilityEpoch, value.ModelRevision = "next", "next" },
		"latency":    func(value *Profile) { value.Latency = LatencySlow },
		"binding":    func(value *Profile) { value.SecretBinding = "secret:other" },
		"timeout":    func(value *Profile) { value.RequestTimeout += time.Second },
		"candidates": func(value *Profile) { value.MaxCandidates-- },
		"query":      func(value *Profile) { value.MaxQueryBytes-- },
		"excerpt":    func(value *Profile) { value.MaxExcerptBytes-- },
		"total":      func(value *Profile) { value.MaxTotalExcerptBytes-- },
		"request":    func(value *Profile) { value.MaxRequestBytes-- },
		"response":   func(value *Profile) { value.MaxResponseBytes-- },
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
	defaults := testProfile(LatencyFast)
	defaults.Latency = ""
	defaults.RequestTimeout = 0
	defaults.MaxCandidates = 0
	defaults.MaxQueryBytes = 0
	defaults.MaxExcerptBytes = 0
	defaults.MaxTotalExcerptBytes = 0
	defaults.MaxRequestBytes = 0
	defaults.MaxResponseBytes = 0
	client, err := New(defaults, testSecrets{"secret:zeroentropy-rerank": "synthetic-key"},
		testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)
	assert.Equal(t, LatencyAuto, client.profile.Latency)
	assert.Equal(t, 30*time.Second, client.profile.RequestTimeout)
	assert.Equal(t, 100, client.profile.MaxCandidates)
	assert.Equal(t, 4096, client.profile.MaxQueryBytes)
	assert.Equal(t, 64<<10, client.profile.MaxExcerptBytes)
	assert.Equal(t, int64(4<<20), client.profile.MaxTotalExcerptBytes)
	assert.Equal(t, int64(8<<20), client.profile.MaxRequestBytes)
	assert.Equal(t, int64(8<<20), client.profile.MaxResponseBytes)

	maximums := testProfile(LatencyFast)
	maximums.RequestTimeout = 5 * time.Minute
	maximums.MaxCandidates = 2048
	maximums.MaxQueryBytes = 64 << 10
	maximums.MaxExcerptBytes = 1 << 20
	maximums.MaxTotalExcerptBytes = 5_000_000
	maximums.MaxRequestBytes = 16 << 20
	maximums.MaxResponseBytes = 64 << 20
	_, err = New(maximums, testSecrets{"secret:zeroentropy-rerank": "synthetic-key"},
		testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)

	for name, mutate := range map[string]func(*Profile){
		"timeout":        func(profile *Profile) { profile.RequestTimeout = 5*time.Minute + time.Nanosecond },
		"candidates":     func(profile *Profile) { profile.MaxCandidates = 2049 },
		"query":          func(profile *Profile) { profile.MaxQueryBytes = 64<<10 + 1 },
		"excerpt":        func(profile *Profile) { profile.MaxExcerptBytes = 1<<20 + 1 },
		"total excerpts": func(profile *Profile) { profile.MaxTotalExcerptBytes = 5_000_001 },
		"request":        func(profile *Profile) { profile.MaxRequestBytes = 16<<20 + 1 },
		"response":       func(profile *Profile) { profile.MaxResponseBytes = 64<<20 + 1 },
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
	profile := testProfile(LatencyFast)
	profile.EgressPolicy.TLS.SPKISHA256 = []string{strings.Repeat("0", 64)}
	suppliedTransport := &inertTransport{}
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	suppliedRedirect := func(*http.Request, []*http.Request) error { return nil }
	supplied := &http.Client{Transport: suppliedTransport, Jar: jar, CheckRedirect: suppliedRedirect, Timeout: time.Minute}

	client, err := New(profile, testSecrets{"secret:zeroentropy-rerank": "synthetic-key"},
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

	wantCIDRs := slices.Clone(client.profile.EgressPolicy.AllowedCIDRs)
	wantPins := slices.Clone(client.profile.EgressPolicy.TLS.SPKISHA256)
	profile.EgressPolicy.AllowedCIDRs[0] = netip.MustParsePrefix("198.51.100.0/24")
	profile.EgressPolicy.TLS.SPKISHA256[0] = strings.Repeat("f", 64)
	assert.Equal(t, wantCIDRs, client.profile.EgressPolicy.AllowedCIDRs)
	assert.Equal(t, wantPins, client.profile.EgressPolicy.TLS.SPKISHA256)
}

func testProfile(latency Latency) Profile {
	return Profile{ID: "zeroentropy-rerank", Model: Model, CompatibilityEpoch: "deployment-2026-08",
		ModelRevision: "deployment-2026-08", SecretBinding: "secret:zeroentropy-rerank", Latency: latency,
		RequestTimeout: time.Second, MaxCandidates: 2048, MaxQueryBytes: 4096,
		MaxExcerptBytes: 64 << 10, MaxTotalExcerptBytes: 4 << 20,
		MaxRequestBytes: 8 << 20, MaxResponseBytes: 8 << 20,
		EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.zeroentropy.dev", Port: 443,
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

type testResolver []netip.Addr

func (resolver testResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), resolver...), nil
}

type inertTransport struct{}

func (*inertTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("ambient transport must not be called")
}
