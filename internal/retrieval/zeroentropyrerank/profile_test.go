package zeroentropyrerank

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
