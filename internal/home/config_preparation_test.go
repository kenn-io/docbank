package home

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecureConfigDoesNotInitializeVault(t *testing.T) {
	missing := Layout{Root: filepath.Join(t.TempDir(), "not-created")}
	require.NoError(t, missing.SecureConfig())
	require.NoDirExists(t, missing.Root)

	layout := Layout{Root: t.TempDir()}
	path := filepath.Join(layout.Root, "config.toml")
	body := []byte("[server]\napi_port=0\n")
	require.NoError(t, os.WriteFile(path, body, 0600))
	require.NoError(t, layout.SecureConfig())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, body, got)
	require.NoFileExists(t, layout.DBPath())
	require.NoDirExists(t, layout.BlobsDir())
}
