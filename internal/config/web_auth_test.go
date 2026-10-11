package config

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestWebLoginConfig(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`[server]
api_key = "synthetic-key-0123456789abcdef0123456789"
[web]
public_origin = "https://Archive.example.test:443"
allowed_hosts = ["backend.example.test:7777"]
session_lifetime = "24h"
`), 0o600))
	cfg, err := Load(root)
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	origin, err := CanonicalWebOrigin(cfg.Web.PublicOrigin)
	require.NoError(t, err)
	require.Equal(t, "https://archive.example.test", origin)
}
