package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/client"
)

func TestMCPCommandExposesOnlyTransportListenAndProcessingFlags(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"mcp"})
	require.NoError(t, err)
	require.Equal(t, "mcp", command.Name())
	var names []string
	command.Flags().VisitAll(func(flag *pflag.Flag) { names = append(names, flag.Name) })
	assert.ElementsMatch(t, []string{"allow-processing", "listen", "transport"}, names)
	for _, forbidden := range []string{"token", "api-key", "daemon", "url", "remote"} {
		assert.Nil(t, command.Flags().Lookup(forbidden))
	}
}

func TestMCPCommandValidatesTransportSpecificOptionsBeforeStarting(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"mcp", "--transport", "invalid"}, want: "stdio or http"},
		{args: []string{"mcp", "--transport", "http"}, want: "--listen is required"},
		{args: []string{"mcp", "--transport", "http", "--listen", "0.0.0.0:7341"}, want: "loopback"},
		{args: []string{"mcp", "--transport", "stdio", "--listen", "127.0.0.1:7341"}, want: "only valid"},
	}
	for _, test := range tests {
		t.Run(test.want, func(t *testing.T) {
			_, err := runCLI(t, test.args...)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestResolveMCPHTTPBearerReadsNamedBindingOncePerStartup(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[mcp.http]
credential_binding = "credential:mcp-http"

[credential_bindings.mcp-http]
environment_variable = "DOCBANK_TEST_MCP_HTTP_TOKEN"
`), 0o600))
	t.Setenv("DOCBANK_TEST_MCP_HTTP_TOKEN", "first-start-token")

	first, err := resolveMCPHTTPBearer(home)
	require.NoError(t, err)
	assert.Equal(t, "first-start-token", first)
	t.Setenv("DOCBANK_TEST_MCP_HTTP_TOKEN", "second-start-token")
	assert.Equal(t, "first-start-token", first, "a running process must keep its startup credential")
	second, err := resolveMCPHTTPBearer(home)
	require.NoError(t, err)
	assert.Equal(t, "second-start-token", second, "a restarted process must resolve the binding again")
}

func TestResolveMCPHTTPBearerRefusesMissingOrEmptyConfigurationWithoutSecretEcho(t *testing.T) {
	const sensitive = "synthetic-secret-must-not-appear"
	tests := []struct {
		name   string
		config string
		value  *string
	}{
		{name: "missing binding"},
		{name: "undefined binding", config: "[mcp.http]\ncredential_binding='credential:mcp-http'\n"},
		{name: "empty environment", config: "[mcp.http]\ncredential_binding='credential:mcp-http'\n" +
			"[credential_bindings.mcp-http]\nenvironment_variable='DOCBANK_TEST_MCP_EMPTY'\n", value: new("")},
		{name: "control byte", config: "[mcp.http]\ncredential_binding='credential:mcp-http'\n" +
			"[credential_bindings.mcp-http]\nenvironment_variable='DOCBANK_TEST_MCP_EMPTY'\n",
			value: new(sensitive + "\ncontinuation")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if test.config != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(test.config), 0o600))
			}
			if test.value != nil {
				t.Setenv("DOCBANK_TEST_MCP_EMPTY", *test.value)
			}
			_, err := resolveMCPHTTPBearer(home)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), sensitive)
		})
	}
}

func TestMCPHTTPStartupRejectsTheEffectiveDaemonKeyAfterAcquisition(t *testing.T) {
	tests := []struct {
		name          string
		startupConfig string
		runtimeToken  func(t *testing.T, home string) string
		finalServer   string
	}{
		{
			name: "ephemeral key from empty server config",
			runtimeToken: func(t *testing.T, home string) string {
				t.Helper()
				records, err := client.RuntimeStore(home).List()
				require.NoError(t, err)
				require.Len(t, records, 1)
				return records[0].Metadata["api_key"]
			},
		},
		{
			name:          "already-running key differs from current config",
			startupConfig: "[server]\napi_key='effective-running-key'\n",
			runtimeToken: func(*testing.T, string) string {
				return "effective-running-key"
			},
			finalServer: "[server]\napi_key='different-current-config-key'\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("DOCBANK_HOME", home)
			if test.startupConfig != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
					[]byte(test.startupConfig), 0o600))
			}
			startTestDaemon(t, home)
			effectiveKey := test.runtimeToken(t, home)
			require.NotEmpty(t, effectiveKey)
			t.Setenv("DOCBANK_TEST_MCP_HTTP_TOKEN", effectiveKey)
			configBody := test.finalServer + `[mcp.http]
credential_binding = "credential:mcp-http"

[credential_bindings.mcp-http]
environment_variable = "DOCBANK_TEST_MCP_HTTP_TOKEN"
`
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600))

			_, err := runCLI(t, "mcp", "--transport", "http", "--listen", "127.0.0.1:0")

			require.ErrorContains(t, err, "must differ from the daemon API key")
			assert.NotContains(t, err.Error(), effectiveKey)
			assert.NotContains(t, err.Error(), "different-current-config-key")
			_, _, running, findErr := client.Find(context.Background(), home)
			require.NoError(t, findErr)
			assert.True(t, running)
		})
	}
}
