package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureStoreBindingsIgnoresStartupAPIKeyFile(t *testing.T) {
	clearServerEnvironment(t)
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	configContents := "[store_bindings.primary]\nkind = \"filesystem\"\npath = \"store\"\n"
	require.NoError(t, os.WriteFile(configPath, []byte(configContents), 0o600))
	t.Setenv("DOCBANK_API_KEY_FILE", filepath.Join(root, "startup-key-not-mounted-in-restore-target"))

	require.NoError(t, EnsureStoreBindings(root, map[string]StoreBindingConfig{
		"primary": {Kind: "filesystem", Path: filepath.Join(root, "store")},
	}))

	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, configContents, string(after))
}
