package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
	root := t.TempDir()
	text := "[integrations.kata]\nenabled=true\nendpoint='http://kata:7777'\napi_key_env='DKE_TEST_KATA_KEY'\ntrust_private_network=true"
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(text), 0600))
	cfg, err := Load(root)
	require.NoError(t, err)
	connection, err := cfg.Integrations.Kata.Resolve(root)
	require.NoError(t, err)
	require.Equal(t, "http://kata:7777", connection.Endpoint)
}

func kataEndpointConfig(t require.TestingT, endpoint string, trust bool) KataConfig {
	cfg := KataConfig{Enabled: true, Endpoint: endpoint, APIKeyEnv: "DKE_TEST_KATA_KEY"}
	require.NoError(t, cfg.UnmarshalTOML(map[string]any{"trust_private_network": trust}))
	return cfg
}

func TestKataEndpointPolicyBoundaries(t *testing.T) {
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	for _, tc := range []struct {
		endpoint string
		trust    bool
		allowed  bool
	}{
		{"http://localhost:7777", false, true},
		{"http://127.0.0.1:7777", false, true},
		{"http://kata:7777", false, false},
		{"http://kata:7777", true, true},
		{"http://8.8.8.8:7777", true, false},
		{"https://8.8.8.8:443", true, true},
	} {
		t.Run(fmt.Sprintf("%s/trust=%t", tc.endpoint, tc.trust), func(t *testing.T) {
			got, err := kataEndpointConfig(t, tc.endpoint, tc.trust).Resolve("")
			if tc.allowed {
				require.NoError(t, err)
				require.Equal(t, tc.endpoint, got.Endpoint)
			} else {
				require.ErrorContains(t, err, "HTTP endpoint must be loopback")
				if tc.trust {
					require.ErrorContains(t, err, "public IPs require HTTPS")
				}
			}
		})
	}
}

func TestKataEndpointRestrictionsAndNormalization(t *testing.T) {
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	for _, endpoint := range []string{
		"https://user:synthetic-secret@service.test", // betterleaks:allow (synthetic credential in a rejected URL)
		"https://example.com/?token=synthetic-secret", "https://example.com/#synthetic-secret",
		"http://kata:7777?", "http://kata:7777/?key=synthetic-secret",
		"http://kata:7777/#synthetic-secret", "http://kata:7777/prefix", "http://kata:7777/%2f", "ftp://kata:7777", "http:opaque",
	} {
		_, err := kataEndpointConfig(t, endpoint, true).Resolve("")
		require.ErrorContains(t, err, "omit credentials, query, fragment, and path")
		require.NotContains(t, err.Error(), "synthetic-secret")
	}
	for _, tc := range []struct{ raw, want string }{
		{" HTTPS://Example.test:443/ ", "https://Example.test:443"},
		{" HTTP://LOCALHOST:80/ ", "http://LOCALHOST:80"},
	} {
		got, err := kataEndpointConfig(t, tc.raw, true).Resolve("")
		require.NoError(t, err)
		require.Equal(t, tc.want, got.Endpoint)
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
	for _, project := range []string{"50%", "has\ttab", "has\x01control", "has\x7fdelete"} {
		cfg := KataConfig{Enabled: true, Endpoint: "http://127.0.0.1:7777", APIKeyEnv: "DKE_TEST_KATA_KEY", DefaultProject: project}
		_, err := cfg.Resolve(t.TempDir())
		require.ErrorContains(t, err, "integrations.kata: invalid default_project")
	}
}
