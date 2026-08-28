package mistral

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/providerhttp"
)

type mistralEmbeddingResolver struct{ address netip.Addr }

func (resolver mistralEmbeddingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{resolver.address}, nil
}

func mistralFixture(t *testing.T, handler http.Handler) (string, providerhttp.EgressPolicy, providerhttp.Resolver) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	_, portText, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	return "http://mistral.invalid:" + portText, providerhttp.EgressPolicy{
		Scheme: "http", Host: "mistral.invalid", Port: uint16(port),
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, ProxyMode: providerhttp.ProxyDisabled,
	}, mistralEmbeddingResolver{address: netip.MustParseAddr("127.0.0.1")}
}

func TestHostedMistralAliasIsAlwaysExportOnlyAndSealed(t *testing.T) {
	endpoint, egress, resolver := mistralFixture(t, http.NotFoundHandler())
	profile := testEmbeddingProfile(t)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile.Descriptor.SupportsTextQuery = true
	_, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "synthetic-secret"}, resolver)
	require.ErrorContains(t, err, "export-only")
}
