//go:build cap_probe

package capselfhosted_test

import (
	"context"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/capselfhosted"
	"go.kenn.io/docbank/document/providerhttp"
)

func TestLiveSelfHostedCapProbe(t *testing.T) {
	origin := os.Getenv("DOCBANK_CAP_TEST_ORIGIN")
	if origin == "" {
		t.Skip("manual-owner-proof not run: no operator deployment")
	}
	parsed, err := url.Parse(origin)
	require.NoError(t, err)
	port := uint16(443)
	if parsed.Scheme == "http" {
		port = 80
	}
	if parsed.Port() != "" {
		value, parseErr := strconv.ParseUint(parsed.Port(), 10, 16)
		require.NoError(t, parseErr)
		port = uint16(value)
	}
	cidr, err := netip.ParsePrefix(os.Getenv("DOCBANK_CAP_TEST_CIDR"))
	require.NoError(t, err)
	policy := providerhttp.EgressPolicy{Scheme: parsed.Scheme, Host: parsed.Hostname(), Port: port,
		AllowedCIDRs: []netip.Prefix{cidr}, ProxyMode: providerhttp.ProxyDisabled,
		TLS: providerhttp.TLSPolicy{SPKISHA256: splitPins(os.Getenv("DOCBANK_CAP_TEST_SPKI"))}}
	client, err := capselfhosted.NewClient(capselfhosted.Deployment{Origin: origin, Egress: policy,
		CredentialBinding: "credential:cap-live", DeploymentRevision: "live", ProbeTimeout: time.Minute},
		liveCapSecrets{}, nil)
	require.NoError(t, err)
	evidence, err := client.Probe(t.Context())
	require.NoError(t, err)
	require.Equal(t, capselfhosted.ProbeVerified, evidence.State)
}

func splitPins(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

type liveCapSecrets struct{}

func (liveCapSecrets) ResolveSecret(context.Context, string) (string, error) {
	return os.Getenv("DOCBANK_CAP_TEST_KEY"), nil
}
