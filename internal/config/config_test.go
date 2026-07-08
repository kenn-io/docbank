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
}

func TestLoadParsesFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[server]\nbind_addr = \"127.0.0.1\"\napi_port = 8080\napi_key = \"k\"\n"+
			"idle_timeout = \"0\"\n[web]\nenabled = false\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, 8080, c.Server.APIPort)
	assert.Equal(t, "k", c.Server.APIKey)
	assert.Equal(t, time.Duration(0), c.Server.IdleTimeout.Std())
	assert.False(t, c.Web.Enabled)
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
		{"private keyless", "192.168.1.5", "", true},
		{"private with key", "192.168.1.5", "k", false},
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
