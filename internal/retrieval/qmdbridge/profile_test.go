package qmdbridge

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/providerhttp"
)

func TestNewDefaultsAndSealsOwnedConfiguration(t *testing.T) {
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	ambientTransport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	supplied := &http.Client{Transport: ambientTransport, Jar: jar, Timeout: time.Hour,
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	profile := validProfile()
	profile.EgressPolicy.Scheme = "https"
	profile.EgressPolicy.Port = 443
	allowed := append([]netip.Prefix(nil), profile.EgressPolicy.AllowedCIDRs...)
	pins := []string{strings.Repeat("a", 64)}
	profile.EgressPolicy.TLS.SPKISHA256 = pins
	client, err := New(profile, secretStub{}, &authorizerStub{}, &authorityStub{},
		canonicalTempRoot(t), resolverStub{}, supplied)
	require.NoError(t, err)

	profile.EgressPolicy.AllowedCIDRs[0] = netip.MustParsePrefix("192.0.2.0/24")
	pins[0] = strings.Repeat("b", 64)
	assert.Equal(t, allowed, client.profile.EgressPolicy.AllowedCIDRs)
	assert.Equal(t, strings.Repeat("a", 64), client.profile.EgressPolicy.TLS.SPKISHA256[0])
	assert.Equal(t, 30*time.Second, client.profile.RequestTimeout)
	assert.Equal(t, 4096, client.profile.MaxQueryBytes)
	assert.Equal(t, 4096, client.profile.MaxIntentBytes)
	assert.Equal(t, 100, client.profile.MaxCandidates)
	assert.Equal(t, 4096, client.profile.MaxSnippetBytes)
	assert.Equal(t, int64(1<<20), client.profile.MaxRequestBytes)
	assert.Equal(t, int64(8<<20), client.profile.MaxResponseBytes)
	assert.NotSame(t, supplied, client.http)
	assert.NotSame(t, ambientTransport, client.http.Transport)
	assert.Nil(t, client.http.Jar)
	assert.Zero(t, client.http.Timeout)
	require.ErrorIs(t, client.http.CheckRedirect(nil, nil), http.ErrUseLastResponse)
}

func TestNewRejectsInvalidDependenciesRootsProfilesAndEndpoints(t *testing.T) {
	validRoot := canonicalTempRoot(t)
	valid := validProfile()
	tests := map[string]func() (*Client, error){
		"nil HTTP client": func() (*Client, error) {
			return New(valid, secretStub{}, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, nil)
		},
		"typed nil secret": func() (*Client, error) {
			var value *secretStub
			return New(valid, value, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, &http.Client{})
		},
		"relative root": func() (*Client, error) {
			return New(valid, secretStub{}, &authorizerStub{}, &authorityStub{}, "relative", resolverStub{}, &http.Client{})
		},
		"volume root": func() (*Client, error) {
			return New(valid, secretStub{}, &authorizerStub{}, &authorityStub{}, filepath.VolumeName(validRoot)+string(filepath.Separator), resolverStub{}, &http.Client{})
		},
		"token space": func() (*Client, error) {
			p := valid
			p.ID = "bad id"
			return New(p, secretStub{}, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, &http.Client{})
		},
		"token control": func() (*Client, error) {
			p := valid
			p.SecretBinding = "bad\n"
			return New(p, secretStub{}, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, &http.Client{})
		},
		"token oversized": func() (*Client, error) {
			p := valid
			p.CompatibilityEpoch = strings.Repeat("x", 1025)
			return New(p, secretStub{}, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, &http.Client{})
		},
		"endpoint query": func() (*Client, error) {
			p := valid
			p.EndpointPath = "/query?x=1"
			return New(p, secretStub{}, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, &http.Client{})
		},
		"endpoint fragment": func() (*Client, error) {
			p := valid
			p.EndpointPath = "/query#x"
			return New(p, secretStub{}, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, &http.Client{})
		},
		"endpoint backslash": func() (*Client, error) {
			p := valid
			p.EndpointPath = `/query\x`
			return New(p, secretStub{}, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, &http.Client{})
		},
		"endpoint noncanonical": func() (*Client, error) {
			p := valid
			p.EndpointPath = "/a/../query"
			return New(p, secretStub{}, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, &http.Client{})
		},
		"endpoint oversized": func() (*Client, error) {
			p := valid
			p.EndpointPath = "/" + strings.Repeat("x", 256)
			return New(p, secretStub{}, &authorizerStub{}, &authorityStub{}, validRoot, resolverStub{}, &http.Client{})
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) { _, err := run(); require.Error(t, err) })
	}
}

func TestNewRejectsTypedNilResolverButRetainsLiteralNilDefault(t *testing.T) {
	root := canonicalTempRoot(t)
	profile := validProfile()
	var typedNil *resolverStub
	_, err := New(profile, secretStub{}, &authorizerStub{}, &authorityStub{}, root, typedNil, &http.Client{})
	require.Error(t, err)
	client, err := New(profile, secretStub{}, &authorizerStub{}, &authorityStub{}, root, nil, &http.Client{})
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestNormalizeProfileRejectsEveryMaximumPlusOne(t *testing.T) {
	tests := []func(*Profile){
		func(p *Profile) { p.RequestTimeout = maximumTimeout + time.Nanosecond },
		func(p *Profile) { p.MaxQueryBytes = maximumQueryBytes + 1 },
		func(p *Profile) { p.MaxIntentBytes = maximumIntentBytes + 1 },
		func(p *Profile) { p.MaxCandidates = maximumCandidates + 1 },
		func(p *Profile) { p.MaxSnippetBytes = maximumSnippetBytes + 1 },
		func(p *Profile) { p.MaxRequestBytes = maximumRequestBytes + 1 },
		func(p *Profile) { p.MaxResponseBytes = maximumResponseBytes + 1 },
	}
	for index, mutate := range tests {
		p := validProfile()
		mutate(&p)
		_, err := normalizeProfile(p)
		require.Error(t, err, "case %d", index)
	}
}

type failingResolver struct{ err error }

func (r failingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, r.err
}

func TestNewRejectsInvalidEgressPolicyWithoutCallingResolver(t *testing.T) {
	p := validProfile()
	p.EgressPolicy.AllowedCIDRs = nil
	_, err := New(p, secretStub{}, &authorizerStub{}, &authorityStub{}, canonicalTempRoot(t),
		failingResolver{err: errors.New("must not resolve")}, &http.Client{})
	require.ErrorContains(t, err, "egress")
}

func validProfile() Profile {
	return Profile{ID: "qmd-test", CompatibilityEpoch: "qmd-current", SecretBinding: "secret:qmd",
		EndpointPath: "/query", EgressPolicy: providerhttp.EgressPolicy{Scheme: "http", Host: "qmd.test", Port: 8080,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, ProxyMode: providerhttp.ProxyDisabled}}
}

func canonicalTempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return filepath.Join(root, "qmd")
}
