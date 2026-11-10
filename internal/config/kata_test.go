package config

import (
	"encoding/binary"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/kit/safefileio"
)

func TestKataConfigErrorsOnlyBlockIntegrationResolution(t *testing.T) {
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	for _, tc := range []struct {
		name      string
		section   string
		wantError string
	}{
		{"invalid_endpoint", "enabled=true\nendpoint='invalid'", "invalid endpoint"},
		{"disabled", "enabled=false\nendpoint='invalid'", "kata integration is disabled"},
		{"invalid_type", "enabled='yes'", "invalid field or type"},
		{"unknown_field", "enabled=true\nsurprise='value'", "invalid field or type"},
		{"literal_key", "enabled=true\napi_key='literal-secret'", "invalid field or type"},
		{"trust_string", "enabled=true\ntrust_private_network='true'", "invalid field or type"},
		{"trust_number", "enabled=true\ntrust_private_network=1", "invalid field or type"},
		{"trust_array", "enabled=true\ntrust_private_network=[true]", "invalid field or type"},
		{"trust_table", "enabled=true\ntrust_private_network={value=true}", "invalid field or type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			text := "[integrations.kata]\napi_key_env='DKE_TEST_KATA_KEY'\n" + tc.section + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(text), 0600))
			cfg, err := Load(root)
			require.NoError(t, err)
			require.NoError(t, cfg.Validate())
			_, err = cfg.Integrations.Kata.Resolve(root)
			require.ErrorContains(t, err, tc.wantError)
			require.NotContains(t, err.Error(), "literal-secret")
		})
	}
}

func TestKataPrivateNetworkTOML(t *testing.T) {
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	for _, trust := range []string{"", "trust_private_network=false", "trust_private_network=true"} {
		t.Run(trust, func(t *testing.T) {
			root := t.TempDir()
			text := "[integrations.kata]\nenabled=true\nendpoint='http://kata:7777'\napi_key_env='DKE_TEST_KATA_KEY'\n" + trust
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(text), 0600))
			cfg, err := Load(root)
			require.NoError(t, err)
			require.NoError(t, cfg.Validate())
			connection, err := cfg.Integrations.Kata.Resolve(root)
			if trust == "trust_private_network=true" {
				require.NoError(t, err)
				require.Equal(t, "http://kata:7777", connection.Endpoint)
			} else {
				require.ErrorContains(t, err, "trust_private_network = true")
			}
		})
	}
}

func kataEndpointConfig(t require.TestingT, endpoint string, trust bool) KataConfig {
	cfg := KataConfig{Enabled: true, Endpoint: endpoint, APIKeyEnv: "DKE_TEST_KATA_KEY"}
	require.NoError(t, cfg.UnmarshalTOML(map[string]any{"trust_private_network": trust}))
	return cfg
}

func TestKataHTTPEndpointAddressPolicy(t *testing.T) {
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	// Prefix membership is an independent oracle for Kit's documented address classes.
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	trusted := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("0.0.0.0/32"),
		netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("::/128"),
	}
	hegel.Test(t, func(ht *hegel.T) {
		var raw [16]byte
		binary.BigEndian.PutUint64(raw[:8], hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64)))
		binary.BigEndian.PutUint64(raw[8:], hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64)))
		ip := netip.AddrFrom16(raw)
		if hegel.Draw(ht, hegel.Booleans()) {
			ip = netip.AddrFrom4([4]byte(raw[:4]))
		}
		trust := hegel.Draw(ht, hegel.Booleans())
		allowed := false
		for _, prefix := range loopback {
			allowed = allowed || prefix.Contains(ip.Unmap())
		}
		for _, prefix := range trusted {
			allowed = allowed || trust && prefix.Contains(ip.Unmap())
		}
		endpoint := "http://" + net.JoinHostPort(ip.String(), "7777")
		got, err := kataEndpointConfig(ht, endpoint, trust).Resolve("")
		if allowed {
			require.NoError(ht, err, "endpoint=%s trust=%v", endpoint, trust)
			require.Equal(ht, endpoint, got.Endpoint)
		} else {
			require.Error(ht, err, "endpoint=%s trust=%v", endpoint, trust)
		}
	}, hegel.WithTestCases(2000))
}

func TestKataIPv6ZoneDoesNotChangeAddressPolicy(t *testing.T) {
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	hegel.Test(t, func(ht *hegel.T) {
		var raw [16]byte
		binary.BigEndian.PutUint64(raw[:8], hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64)))
		binary.BigEndian.PutUint64(raw[8:], hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64)))
		ip := netip.AddrFrom16(raw).String()
		zone := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789").MinSize(1).MaxSize(16))
		trust := hegel.Draw(ht, hegel.Booleans())
		plain := (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip, "7777")}).String()
		scoped := (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip+"%"+zone, "7777")}).String()
		_, plainErr := kataEndpointConfig(ht, plain, trust).Resolve("")
		got, scopedErr := kataEndpointConfig(ht, scoped, trust).Resolve("")
		require.Equal(ht, plainErr == nil, scopedErr == nil)
		if plainErr == nil {
			require.Equal(ht, scoped, got.Endpoint)
		}
	}, hegel.WithTestCases(2000))
}

func TestKataEndpointPolicyBoundaries(t *testing.T) {
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	for _, host := range []string{"localhost", "LOCALHOST", "127.0.0.1", "127.255.255.255", "::1", "::ffff:127.0.0.1"} {
		for _, trust := range []bool{false, true} {
			endpoint := "http://" + net.JoinHostPort(host, "7777")
			got, err := kataEndpointConfig(t, endpoint, trust).Resolve("")
			require.NoError(t, err)
			require.Equal(t, endpoint, got.Endpoint)
		}
	}
	for _, host := range []string{
		"kata", "A.test", "service.example.test", "localhost.", "a.localhost", "10.0.0.0", "10.255.255.255",
		"172.16.0.0", "172.31.255.255", "192.168.0.0", "192.168.255.255", "169.254.0.0", "169.254.255.255",
		"0.0.0.0", "100.64.0.0", "100.127.255.255", "fc00::", "fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
		"fe80::", "febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "::", "::ffff:10.0.0.1", "::ffff:100.64.0.1", "fe80::1%eth0",
	} {
		endpoint := (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, "7777")}).String()
		for _, trust := range []bool{false, true} {
			cfg := kataEndpointConfig(t, endpoint, trust)
			got, err := cfg.Resolve("")
			if trust {
				require.NoError(t, err, "%s", endpoint)
				require.Equal(t, endpoint, got.Endpoint)
			} else {
				require.ErrorContains(t, err, "trust_private_network = true")
			}
		}
	}
	for _, host := range []string{
		"", "8.8.8.8", "9.255.255.255", "11.0.0.0", "100.63.255.255", "100.128.0.0", "172.15.255.255", "172.32.0.0",
		"192.167.255.255", "192.169.0.0", "169.253.255.255", "169.255.0.0", "0.0.0.1", "255.255.255.255",
		"fbff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "fe00::", "fe7f:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "fec0::",
		"2001:4860:4860::8888", "224.0.0.1", "ff02::1", "::ffff:8.8.8.8", "::ffff:808:808",
	} {
		for _, trust := range []bool{false, true} {
			_, err := kataEndpointConfig(t, "http://"+net.JoinHostPort(host, "7777"), trust).Resolve("")
			require.Error(t, err, "host=%q trust=%v", host, trust)
		}
	}
}

func TestKataEndpointRestrictionsAndNormalization(t *testing.T) {
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	for _, trust := range []bool{false, true} {
		for _, endpoint := range []string{
			"https://user:synthetic-secret@service.test", // betterleaks:allow (synthetic credential in a rejected URL)
			"http://kata:7777?", "http://kata:7777/?key=synthetic-secret",
			"http://kata:7777/#synthetic-secret", "http://kata:7777/prefix", "http://kata:7777/%2f", "ftp://kata:7777", "http:opaque",
		} {
			_, err := kataEndpointConfig(t, endpoint, trust).Resolve("")
			require.Error(t, err)
			require.NotContains(t, err.Error(), "synthetic-secret")
		}
		for _, tc := range []struct{ raw, want string }{
			{" HTTPS://Example.test:443/ ", "https://Example.test:443"},
			{" HTTP://LOCALHOST:80/ ", "http://LOCALHOST:80"},
			{"https://8.8.8.8:443", "https://8.8.8.8:443"},
		} {
			got, err := kataEndpointConfig(t, tc.raw, trust).Resolve("")
			require.NoError(t, err, "trust=%v", trust)
			require.Equal(t, tc.want, got.Endpoint)
		}
	}
}

func TestKataCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	cfg := KataConfig{Enabled: true, Endpoint: "http://127.0.0.1:7777", APIKeyEnv: "DKE_TEST_KATA_KEY"}
	got, err := cfg.Resolve(root)
	require.NoError(t, err)
	require.Equal(t, "synthetic-token", got.APIKey)
	require.Equal(t, "docbank", got.Project)
	cfg.APIKeyEnv, cfg.APIKeyFile = "", "kata-key"
	keyFile := filepath.Join(root, "kata-key")
	file, err := safefileio.CreatePrivateFile(keyFile)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, os.WriteFile(keyFile, []byte("synthetic-file-token\n"), 0600))
	got, err = cfg.Resolve(root)
	require.NoError(t, err)
	require.Equal(t, "synthetic-file-token", got.APIKey)
	require.NoError(t, os.WriteFile(keyFile, []byte("synthetic-file-token"+strings.Repeat(" ", 4096)), 0600))
	_, err = cfg.Resolve(root)
	require.ErrorContains(t, err, "at most 4096 bytes")
	missing := cfg
	missing.APIKeyFile = "missing-key"
	_, err = missing.Resolve(root)
	require.ErrorContains(t, err, "api_key_file is missing or unreadable")
	cfg.APIKeyEnv = "DKE_TEST_KATA_KEY"
	_, err = cfg.Resolve(root)
	require.ErrorContains(t, err, "exactly one")
	cfg.APIKeyFile = ""
	for _, endpoint := range []string{"http://remote.example", "https://user:secret@example.com", "https://example.com/?token=secret", "https://example.com/#secret"} {
		cfg.Endpoint = endpoint
		_, err = cfg.Resolve(root)
		require.ErrorContains(t, err, "endpoint")
		require.NotContains(t, err.Error(), "secret")
	}
	for _, project := range []string{"50%", "has\ttab", "has\x01control", "has\x7fdelete"} {
		cfg := KataConfig{Enabled: true, Endpoint: "http://127.0.0.1:7777", APIKeyEnv: "DKE_TEST_KATA_KEY", DefaultProject: project}
		_, err := cfg.Resolve(t.TempDir())
		require.ErrorContains(t, err, "integrations.kata: invalid default_project")
	}
}
