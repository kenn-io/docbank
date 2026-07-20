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
	assert.False(t, c.Embeddings.Enabled())
	assert.Equal(t, 768, c.Embeddings.Dimensions)
	assert.Equal(t, 32, c.Embeddings.BatchSize)
	assert.Equal(t, 2, c.Embeddings.Concurrency)
	assert.Equal(t, 30*time.Second, c.Embeddings.Timeout.Std())
}

func TestLoadParsesFile(t *testing.T) {
	dir := privateTestConfigDir(t)
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
	dir := privateTestConfigDir(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[backup]\nrepo = \"~/backups\"\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "backups"), c.Backup.Repo)
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[server]\nbindaddr = \"x\"\n"), 0o600))
	_, err := Load(dir)
	require.ErrorContains(t, err, "bindaddr")
}

func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	dir := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[server]\napi_port = 8080\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, 8080, c.Server.APIPort)
	assert.Equal(t, "127.0.0.1", c.Server.BindAddr)
	assert.Equal(t, 30*time.Minute, c.Server.IdleTimeout.Std())
	assert.True(t, c.Web.Enabled)
}

func TestLoadParsesWatchedInboxesAndAppliesDefaults(t *testing.T) {
	dir := privateTestConfigDir(t)
	source := t.TempDir()
	relativeHomeSource := "~/agent-sessions"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[[watch]]\nname = \"documents\"\nsource = \""+filepath.ToSlash(source)+"\"\n"+
			"destination = \"/inbox/documents\"\nexclude = [\".tmp\", \"cache/\"]\n"+
			"settle_time = \"45s\"\nscan_interval = \"3s\"\n\n"+
			"[[watch]]\nname = \"sessions\"\nsource = \""+relativeHomeSource+"\"\n"+
			"destination = \"/agents/sessions\"\n"), 0o600))

	c, err := Load(dir)
	require.NoError(t, err)
	require.Len(t, c.Watches, 2)
	assert.Equal(t, filepath.Clean(source), c.Watches[0].Source)
	assert.Equal(t, "/inbox/documents", c.Watches[0].Destination)
	assert.Equal(t, 45*time.Second, c.Watches[0].SettleTime.Std())
	assert.Equal(t, 3*time.Second, c.Watches[0].ScanInterval.Std())
	assert.Equal(t, []string{".tmp", "cache/"}, c.Watches[0].Exclude)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "agent-sessions"), c.Watches[1].Source)
	assert.Equal(t, 30*time.Second, c.Watches[1].SettleTime.Std())
	assert.Equal(t, 5*time.Second, c.Watches[1].ScanInterval.Std())
	require.NoError(t, c.Validate())
}

func TestWatchedInboxConfigRejectsAmbiguousOrUnsafeValues(t *testing.T) {
	dir := privateTestConfigDir(t)
	source := filepath.ToSlash(t.TempDir())
	for _, tc := range []struct {
		name, body, want string
	}{
		{"relative source", "name='inbox'\nsource='relative'\ndestination='/inbox'", "must be absolute"},
		{"relative destination", "name='inbox'\nsource='" + source + "'\ndestination='inbox'", "absolute virtual path"},
		{"invalid name", "name='Bad Name'\nsource='" + source + "'\ndestination='/inbox'", "unsupported characters"},
		{"duplicate name", "name='same'\nsource='" + source + "'\ndestination='/one'\n" +
			"[[watch]]\nname='same'\nsource='" + filepath.ToSlash(t.TempDir()) + "'\ndestination='/two'", "duplicated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
				[]byte("[[watch]]\n"+tc.body+"\n"), 0o600))
			cfg, err := Load(dir)
			if err == nil {
				err = cfg.Validate()
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
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

func TestValidateEmbeddings(t *testing.T) {
	valid := Default()
	valid.Embeddings.BaseURL = "http://127.0.0.1:11434/v1"
	valid.Embeddings.Model = "nomic-embed-text"
	require.NoError(t, valid.Validate())

	for _, tc := range []struct {
		name string
		edit func(*EmbeddingsConfig)
		want string
	}{
		{"partial", func(e *EmbeddingsConfig) { e.Model = "" }, "both be set"},
		{"plaintext remote", func(e *EmbeddingsConfig) { e.BaseURL = "http://example.com/v1" }, "loopback"},
		{"credentials", func(e *EmbeddingsConfig) { e.BaseURL = "https://user@example.com/v1" }, "credentials"},
		{"key conflict", func(e *EmbeddingsConfig) { e.APIKey, e.APIKeyEnv = "x", "TOKEN" }, "mutually exclusive"},
		{"zero dimensions", func(e *EmbeddingsConfig) { e.Dimensions = 0 }, "dimensions"},
		{"excessive dimensions", func(e *EmbeddingsConfig) { e.Dimensions = 8193 }, "dimensions"},
		{"zero batch", func(e *EmbeddingsConfig) { e.BatchSize = 0 }, "batch_size"},
		{"zero concurrency", func(e *EmbeddingsConfig) { e.Concurrency = 0 }, "concurrency"},
		{"zero timeout", func(e *EmbeddingsConfig) { e.Timeout = 0 }, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.edit(&cfg.Embeddings)
			require.ErrorContains(t, cfg.Validate(), tc.want)
		})
	}
	disabledWithKey := Default()
	disabledWithKey.Embeddings.APIKeyEnv = "TOKEN"
	require.ErrorContains(t, disabledWithKey.Validate(), "require base_url")
}
