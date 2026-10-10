package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/safefileio"
)

func clearServerEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"DOCBANK_BIND_ADDR", "DOCBANK_API_PORT", "DOCBANK_API_KEY", "DOCBANK_API_KEY_FILE", "DOCBANK_ALLOWED_HOSTS", "DOCBANK_MCP_HTTP_TOKEN", "DOCBANK_MCP_HTTP_TOKEN_FILE", "DOCBANK_MCP_HTTP_ALLOWED_HOSTS"} {
		t.Setenv(name, "")
	}
}

func TestStartupEnvironmentOverridesAbsentAndExistingConfig(t *testing.T) {
	for _, withFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "toml"}[withFile], func(t *testing.T) {
			clearServerEnvironment(t)
			root := t.TempDir()
			if withFile {
				require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[server]\napi_port=1234\napi_key='toml-key'\nallowed_hosts=['old.example']\n"), 0o600))
			}
			t.Setenv("DOCBANK_BIND_ADDR", "0.0.0.0")
			t.Setenv("DOCBANK_API_PORT", "8485")
			t.Setenv("DOCBANK_API_KEY", "synthetic-env-key")
			t.Setenv("DOCBANK_ALLOWED_HOSTS", " docbank:8485 , archive.example ")
			cfg, err := Load(root)
			require.NoError(t, err)
			require.NoError(t, cfg.Validate())
			assert.Equal(t, "0.0.0.0", cfg.Server.BindAddr)
			assert.Equal(t, 8485, cfg.Server.APIPort)
			assert.Equal(t, "synthetic-env-key", cfg.Server.APIKey)
			assert.Equal(t, []string{"docbank:8485", "archive.example"}, cfg.Server.AllowedHosts)
			if withFile {
				data, err := os.ReadFile(filepath.Join(root, "config.toml"))
				require.NoError(t, err)
				assert.NotContains(t, string(data), "synthetic-env-key")
			} else {
				_, err := os.Stat(filepath.Join(root, "config.toml"))
				assert.True(t, os.IsNotExist(err))
			}
		})
	}
}

func TestEmptyEnvironmentPreservesTOML(t *testing.T) {
	clearServerEnvironment(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[server]\napi_port=1234\napi_key='toml-key'\nallowed_hosts=['docbank']\n"), 0o600))
	cfg, err := Load(root)
	require.NoError(t, err)
	assert.Equal(t, 1234, cfg.Server.APIPort)
	assert.Equal(t, "toml-key", cfg.Server.APIKey)
	assert.Equal(t, []string{"docbank"}, cfg.Server.AllowedHosts)
}

func TestEnvironmentSecretFilesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name, contents string
		valid          bool
	}{
		{"token", "synthetic-key", true}, {"LF", "synthetic-key\n", true}, {"CRLF", "synthetic-key\r\n", true},
		{"empty", "", false}, {"spaces", "a b", false}, {"two lines", "a\nb\n", false},
		{"limit", strings.Repeat("x", 4096) + "\n", true}, {"oversized", strings.Repeat("x", 4097), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearServerEnvironment(t)
			path := filepath.Join(t.TempDir(), "key")
			writePrivateSecret(t, path, test.contents)
			t.Setenv("DOCBANK_API_KEY_FILE", path)
			cfg, err := Load(t.TempDir())
			if test.valid {
				require.NoError(t, err)
				assert.Equal(t, strings.TrimSuffix(strings.TrimSuffix(test.contents, "\n"), "\r"), cfg.Server.APIKey)
			} else {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "synthetic-key")
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		clearServerEnvironment(t)
		t.Setenv("DOCBANK_API_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
		_, err := Load(t.TempDir())
		require.Error(t, err)
	})
	t.Run("directory", func(t *testing.T) {
		clearServerEnvironment(t)
		t.Setenv("DOCBANK_API_KEY_FILE", t.TempDir())
		_, err := Load(t.TempDir())
		require.Error(t, err)
	})
	t.Run("conflict", func(t *testing.T) {
		clearServerEnvironment(t)
		t.Setenv("DOCBANK_API_KEY", "synthetic-key")
		t.Setenv("DOCBANK_API_KEY_FILE", "missing")
		_, err := Load(t.TempDir())
		require.ErrorContains(t, err, "cannot both")
		assert.NotContains(t, err.Error(), "synthetic-key")
	})
}

func TestNetworkConfigRejectsInvalidSettings(t *testing.T) {
	for _, value := range []string{"-1", "65536", "invalid"} {
		t.Run(value, func(t *testing.T) {
			clearServerEnvironment(t)
			t.Setenv("DOCBANK_API_PORT", value)
			_, err := Load(t.TempDir())
			require.Error(t, err)
		})
	}
	for _, host := range []string{"*", "http://docbank", "docbank:0", "docbank:65536", "", "a,b"} {
		cfg := Default()
		cfg.Server.AllowedHosts = []string{host}
		require.Error(t, cfg.Validate())
	}
	cfg := Default()
	cfg.Server.APIKey = "bad key"
	require.Error(t, cfg.Validate())
	cfg = Default()
	cfg.Server.APIPort = -1
	require.Error(t, cfg.Validate())
}

func TestMCPEnvironmentSelectsBindingWithoutReadingSecretInDaemon(t *testing.T) {
	clearServerEnvironment(t)
	t.Setenv("DOCBANK_MCP_HTTP_TOKEN_FILE", filepath.Join(t.TempDir(), "not-mounted-here"))
	t.Setenv("DOCBANK_MCP_HTTP_ALLOWED_HOSTS", "docbank:7341, archive.example")
	cfg, err := Load(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	assert.Equal(t, "credential:docbank-mcp-http-env", cfg.MCP.HTTP.CredentialBinding)
	assert.Equal(t, "DOCBANK_MCP_HTTP_TOKEN", cfg.CredentialBindings["docbank-mcp-http-env"].EnvironmentVariable)
	assert.Equal(t, []string{"docbank:7341", "archive.example"}, cfg.MCP.HTTP.AllowedHosts)
}

func TestMCPEnvironmentBindingCollision(t *testing.T) {
	for _, environmentVariable := range []string{"DOCBANK_MCP_HTTP_TOKEN", "DOCBANK_MCP_HTTP_TOKEN_FILE"} {
		t.Run(environmentVariable, func(t *testing.T) {
			clearServerEnvironment(t)
			root := t.TempDir()
			configFile := `[credential_bindings.docbank-mcp-http-env]
environment_variable = "DOCBANK_PROVIDER_KEY"

[credential_bindings.docbank-mcp-http-env-1]
environment_variable = "DOCBANK_OTHER_PROVIDER_KEY"
`
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(configFile), 0o600))
			if environmentVariable == "DOCBANK_MCP_HTTP_TOKEN" {
				t.Setenv(environmentVariable, "synthetic-inbound-token")
			} else {
				t.Setenv(environmentVariable, filepath.Join(root, "not-mounted-here"))
			}

			cfg, err := Load(root)
			require.NoError(t, err)
			require.NoError(t, cfg.Validate())
			assert.Equal(t, "credential:docbank-mcp-http-env-2", cfg.MCP.HTTP.CredentialBinding)
			assert.Equal(t, CredentialBindingConfig{EnvironmentVariable: "DOCBANK_PROVIDER_KEY"},
				cfg.CredentialBindings["docbank-mcp-http-env"])
			assert.Equal(t, CredentialBindingConfig{EnvironmentVariable: "DOCBANK_OTHER_PROVIDER_KEY"},
				cfg.CredentialBindings["docbank-mcp-http-env-1"])
			assert.Equal(t, CredentialBindingConfig{EnvironmentVariable: "DOCBANK_MCP_HTTP_TOKEN"},
				cfg.CredentialBindings["docbank-mcp-http-env-2"])
		})
	}
}

func writePrivateSecret(t *testing.T, path, contents string) {
	t.Helper()
	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.WriteString(contents)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}
