package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/safefileio"
)

func TestKataConfigIsScoped(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[integrations.kata]\nenabled=true\nendpoint='invalid'\napi_key_env='DKE_TEST_KATA_KEY'\n"), 0600))
	cfg, err := Load(root)
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	_, err = (KataConfig{}).Resolve(root)
	require.ErrorContains(t, err, "kata integration is disabled")
}

func TestKataMalformedSectionDoesNotBlockUnrelatedCommands(t *testing.T) {
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-token")
	for _, section := range []string{"enabled='yes'", "enabled=true\nsurprise='value'", "enabled=true\napi_key='literal-secret'"} {
		root := t.TempDir()
		text := "[integrations.kata]\nendpoint='http://127.0.0.1:7777'\napi_key_env='DKE_TEST_KATA_KEY'\n" + section + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(text), 0600))
		cfg, err := Load(root)
		require.NoError(t, err)
		require.NoError(t, cfg.Validate())
		_, err = cfg.Integrations.Kata.Resolve(root)
		require.ErrorContains(t, err, "invalid field or type")
		require.NotContains(t, err.Error(), "literal-secret")
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
