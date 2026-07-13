package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	c, err := Load(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", c.Server.BindAddr)
	assert.Equal(t, 0, c.Server.APIPort)
	assert.Empty(t, c.Server.APIKey)
	assert.Equal(t, 30*time.Minute, c.Server.IdleTimeout.Std())
	assert.True(t, c.Web.Enabled)
	assert.Empty(t, c.Backup.Repo)
	assert.Zero(t, c.Backup.ZstdLevel)
}

func TestLoadParsesFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[server]\nbind_addr = \"127.0.0.1\"\napi_port = 8080\napi_key = \"k\"\n"+
			"idle_timeout = \"0\"\n[web]\nenabled = false\n[backup]\nrepo = \"snapshots\"\nzstd_level = 9\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, 8080, c.Server.APIPort)
	assert.Equal(t, "k", c.Server.APIKey)
	assert.Equal(t, time.Duration(0), c.Server.IdleTimeout.Std())
	assert.False(t, c.Web.Enabled)
	assert.Equal(t, filepath.Join(dir, "snapshots"), c.Backup.Repo)
	assert.Equal(t, 9, c.Backup.ZstdLevel)
}

func TestLoadExpandsBackupRepoHome(t *testing.T) {
	dir := t.TempDir()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[backup]\nrepo = \"~/backups\"\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "backups"), c.Backup.Repo)
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[server]\nbindaddr = \"x\"\n"), 0o600))
	_, err := Load(dir)
	require.ErrorContains(t, err, "bindaddr")
}

func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[server]\napi_port = 8080\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, 8080, c.Server.APIPort)
	assert.Equal(t, "127.0.0.1", c.Server.BindAddr)
	assert.Equal(t, 30*time.Minute, c.Server.IdleTimeout.Std())
	assert.True(t, c.Web.Enabled)
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name, bind, key string
		wantErr         bool
	}{
		{"loopback keyless", "127.0.0.1", "", false},
		{"localhost keyless", "localhost", "", false},
		{"ipv6 loopback keyless", "::1", "", false},
		{"loopback with key", "127.0.0.1", "k", false},
		// The API is plain HTTP: every non-loopback bind is refused, keyed
		// or not - a key on the wire in cleartext is not protection.
		{"private keyless", "192.168.1.5", "", true},
		{"private with key", "192.168.1.5", "k", true},
		{"public with key", "203.0.113.9", "k", true},
		{"wildcard keyless", "0.0.0.0", "", true},
		{"wildcard with key", "0.0.0.0", "k", true},
		{"garbage host", "not an ip", "k", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Server.BindAddr, c.Server.APIKey = tc.bind, tc.key
			err := c.Validate()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateBackupCompressionLevel(t *testing.T) {
	for _, level := range []int{0, 1, 19} {
		c := Default()
		c.Backup.ZstdLevel = level
		require.NoError(t, c.Validate())
	}
	for _, level := range []int{-1, 20} {
		c := Default()
		c.Backup.ZstdLevel = level
		require.ErrorContains(t, c.Validate(), "zstd_level")
	}
}
