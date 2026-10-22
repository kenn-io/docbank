package config

import (
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
