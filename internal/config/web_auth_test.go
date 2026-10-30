package config

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strconv"
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

func TestWebBasePathConfig(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "/archive", "/tools/library-2", "/library.v1"} {
		t.Run("valid:"+value, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[web]\nbase_path = "+strconv.Quote(value)+"\n"), 0o600))
			cfg, err := Load(root)
			require.NoError(t, err)
			require.NoError(t, cfg.Validate())
		})
	}
	for _, value := range []string{"/", "archive", "//archive", "/archive/", "/a//b", "/a/../b", "/a/./b", "/a%2fb", "/a?b", "/a#b", "/a\\b", "/a b", "/café", "/api", "/api/nested", "/health", "/docs", "/openapi.json"} {
		t.Run("invalid:"+value, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[web]\nbase_path = "+strconv.Quote(value)+"\n"), 0o600))
			cfg, err := Load(root)
			if err == nil {
				err = cfg.Validate()
			}
			require.ErrorContains(t, err, "base_path")
		})
	}
}
