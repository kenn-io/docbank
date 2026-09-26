package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMCPStdioCancellationWithInheritedPipes(t *testing.T) {
	const childVariable = "DOCBANK_TEST_MCP_STDIO_CHILD"
	if mode := os.Getenv(childVariable); mode != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		wantErr := context.Canceled
		if mode == "deadline" {
			wantErr = context.DeadlineExceeded
		}
		command := &cobra.Command{}
		command.SetContext(ctx)
		mcpTransport = "stdio"
		err := runMCP(command)
		cancel()
		if !errors.Is(err, wantErr) {
			os.Exit(1)
		}
		// The transport owns duplicates, leaving the caller's streams usable.
		if _, err := os.Stdin.Stat(); err != nil {
			os.Exit(1)
		}
		if _, err := io.WriteString(os.Stdout, "shutdown\n"); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	for _, mode := range []string{"deadline", "signal"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "signal" && runtime.GOOS == "windows" {
				t.Skip("os.Process.Signal cannot deliver os.Interrupt on Windows")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			executable, err := os.Executable()
			require.NoError(t, err)
			command := exec.CommandContext(ctx, executable, "-test.run=^TestMCPStdioCancellationWithInheritedPipes$")
			command.Env = append(os.Environ(), childVariable+"="+mode, "DOCBANK_HOME="+t.TempDir())
			input, err := command.StdinPipe()
			require.NoError(t, err)
			defer func() { _ = input.Close() }()
			output, err := command.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, command.Start())
			t.Cleanup(func() { _ = command.Process.Kill() })
			_, err = io.WriteString(input, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
			require.NoError(t, err)
			reader := bufio.NewReader(output)
			response, err := reader.ReadBytes('\n')
			require.NoError(t, err)
			require.Contains(t, string(response), `"result"`, "the command must be serving before cancellation")
			_, err = io.WriteString(input, `{"jsonrpc":`)
			require.NoError(t, err)
			if mode == "signal" {
				require.NoError(t, command.Process.Signal(os.Interrupt))
			}
			shutdown, err := reader.ReadString('\n')
			require.NoError(t, err)
			assert.Equal(t, "shutdown\n", shutdown)
			require.NoError(t, command.Wait(), "cancellation must finish while stdin remains open")
		})
	}
}

func TestMCPCommandExposesTransportAndCapabilityFlags(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"mcp"})
	require.NoError(t, err)
	require.Equal(t, "mcp", command.Name())
	var names []string
	command.Flags().VisitAll(func(flag *pflag.Flag) { names = append(names, flag.Name) })
	assert.ElementsMatch(t, []string{"allow-processing", "allow-package-writes", "allow-report-writes", "listen", "transport"}, names)
	for _, forbidden := range []string{"token", "api-key", "daemon", "url", "remote"} {
		assert.Nil(t, command.Flags().Lookup(forbidden))
	}
}

func TestMCPCommandWriteFlagsSelectTools(t *testing.T) {
	const childVariable = "DOCBANK_TEST_MCP_TOOL_CATALOG"
	if args := os.Getenv(childVariable); args != "" {
		rootCmd.SetArgs(strings.Fields(args))
		if err := rootCmd.Execute(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	for _, test := range []struct {
		args                          string
		processing, packages, reports bool
	}{
		{args: "mcp"},
		{args: "mcp --allow-processing", processing: true},
		{args: "mcp --allow-package-writes", packages: true},
		{args: "mcp --allow-report-writes", reports: true},
		{args: "mcp --allow-processing --allow-package-writes", processing: true, packages: true},
	} {
		t.Run(test.args, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			executable, err := os.Executable()
			require.NoError(t, err)
			command := exec.CommandContext(ctx, executable, "-test.run=^TestMCPCommandWriteFlagsSelectTools$")
			command.Env = append(os.Environ(), childVariable+"="+test.args, "DOCBANK_HOME="+t.TempDir())
			input, err := command.StdinPipe()
			require.NoError(t, err)
			defer func() { _ = input.Close() }()
			output, err := command.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, command.Start())
			t.Cleanup(func() { _ = command.Process.Kill() })
			_, err = io.WriteString(input, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
			require.NoError(t, err)
			response, err := bufio.NewReader(output).ReadBytes('\n')
			require.NoError(t, err)
			var catalog struct {
				Result struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"result"`
			}
			require.NoError(t, json.Unmarshal(response, &catalog))
			names := make(map[string]bool, len(catalog.Result.Tools))
			for _, tool := range catalog.Result.Tools {
				names[tool.Name] = true
			}
			assert.True(t, names["get_package_record"], "reads remain available with every flag combination")
			assert.True(t, names["get_report_summary"])
			assert.True(t, names["get_report_dates"])
			assert.Equal(t, test.reports, names["create_report"])
			assert.Equal(t, test.reports, names["revise_report"])
			assert.Equal(t, test.processing, names["start_processing"])
			for _, name := range []string{"preflight_load_file_package", "start_package_import", "resolve_package_custodian", "assign_package_custodian"} {
				assert.Equal(t, test.packages, names[name], name)
			}
			require.NoError(t, input.Close())
			require.NoError(t, command.Wait())
		})
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
				records, err := daemonconn.RuntimeStore(home).List()
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
			_, _, running, findErr := daemonconn.Find(context.Background(), home)
			require.NoError(t, findErr)
			assert.True(t, running)
		})
	}
}
