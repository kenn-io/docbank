package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/api"
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
	assert.ElementsMatch(t, []string{"allow-processing", "allow-package-writes", "allow-export-writes", "allow-report-writes", "listen", "transport"}, names)
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
		args                                   string
		processing, packages, exports, reports bool
	}{
		{args: "mcp"},
		{args: "mcp --allow-processing", processing: true},
		{args: "mcp --allow-package-writes", packages: true},
		{args: "mcp --allow-export-writes", exports: true},
		{args: "mcp --allow-report-writes", reports: true},
		{args: "mcp --allow-export-writes --allow-report-writes", exports: true, reports: true},
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
			assert.True(t, names["preview_export_plan"])
			assert.True(t, names["get_export_job"])
			assert.True(t, names["get_report_summary"])
			assert.True(t, names["get_report_dates"])
			assert.Equal(t, test.reports, names["create_report"])
			assert.Equal(t, test.reports, names["revise_report"])
			assert.Equal(t, test.processing, names["start_processing"])
			for _, name := range []string{"create_export_source", "create_export_plan", "start_export_job", "cancel_export_job"} {
				assert.Equal(t, test.exports, names[name], name)
			}
			for _, name := range []string{"preflight_load_file_package", "start_package_import", "resolve_package_custodian", "assign_package_custodian"} {
				assert.Equal(t, test.packages, names[name], name)
			}
			require.NoError(t, input.Close())
			require.NoError(t, command.Wait())
		})
	}
}

func TestMCPCommandSessionFileNarrowsAdvertisedTools(t *testing.T) {
	const childVariable = "DOCBANK_TEST_MCP_SCOPED_CATALOG"
	if os.Getenv(childVariable) != "" {
		rootCmd.SetArgs([]string{"mcp", "--allow-export-writes", "--allow-report-writes"})
		if err := rootCmd.Execute(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	vaultID := "11111111-1111-4111-8111-111111111111"
	versionID := "22222222-2222-4222-8222-222222222222"
	token := strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, token, r.Header.Get(api.AgentSessionHeader))
		assert.Empty(t, r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/agent/capabilities":
			assert.NoError(t, json.MarshalWrite(w, api.AgentCapabilities{Contract: agentops.Schema,
				Server: agentops.ServerCapabilities{VaultID: vaultID}, Session: &api.AgentSessionProjection{
					SubjectID: "agent:" + strings.Repeat("a", 32), CredentialKind: "agent_session",
					Audience: "docbank:" + vaultID, Operations: []api.Operation{api.OperationRead},
					SourceIDs: []string{versionID}, GrantRevision: 1, ExpiresAt: time.Now().Add(time.Hour),
				}}))
		case "/api/v1/documents/scoped":
			var query api.ScopedDocumentQuery
			assert.NoError(t, json.UnmarshalRead(r.Body, &query))
			assert.Equal(t, []string{versionID}, query.ContentVersionIDs)
			assert.NoError(t, json.MarshalWrite(w, api.DocumentPage{PathPrefix: "/", Sort: "path", Direction: "asc",
				PageSize: 1, Items: []api.DocumentSummary{{NodeID: 7, ContentVersionID: versionID,
					Path: "/synthetic.txt", Name: "synthetic.txt", MediaType: "text/plain",
					ModifiedAt: "2026-09-25T00:00:00Z", ActiveRenditions: []api.DocumentRenditionIdentity{}}}}))
		case "/api/v1/capabilities":
			assert.NoError(t, json.MarshalWrite(w, api.Capabilities{VaultUID: vaultID,
				APIVersion: api.RemoteAPIVersion, Operations: []string{"read"}, Limits: map[string]int64{}}))
		case "/api/v1/versions/" + versionID:
			assert.NoError(t, json.MarshalWrite(w, api.ContentVersion{ID: versionID, NodeID: 7,
				BlobHash: strings.Repeat("c", 64), Size: 12, NodeRevision: 1,
				RecordedAt: "2026-09-25T00:00:00Z", TransitionKind: "content_create"}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	file := filepath.Join(t.TempDir(), "session.json")
	raw, err := json.Marshal(daemonconn.AgentSessionFile{Version: 1, Origin: server.URL,
		Token: token, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestMCPCommandSessionFileNarrowsAdvertisedTools$")
	command.Env = append(os.Environ(), childVariable+"=1", "DOCBANK_HOME="+t.TempDir(),
		"DOCBANK_AGENT_SESSION_FILE="+file)
	input, err := command.StdinPipe()
	require.NoError(t, err)
	defer func() { _ = input.Close() }()
	output, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() { _ = command.Process.Kill() })
	_, err = io.WriteString(input, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
	require.NoError(t, err)
	reader := bufio.NewReader(output)
	response, err := reader.ReadBytes('\n')
	require.NoError(t, err)
	var catalog struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response, &catalog))
	names := make([]string, 0, len(catalog.Result.Tools))
	for _, tool := range catalog.Result.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{"get_agent_capabilities", "get_vault_info", "list_documents", "get_document", "list_tags",
		"read_rendition_text", "list_processing_profiles", "get_format_coverage"}, names)
	_, err = io.WriteString(input, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_documents","arguments":{"page_size":1},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
	require.NoError(t, err)
	response, err = reader.ReadBytes('\n')
	require.NoError(t, err)
	var listed struct {
		Result struct {
			StructuredContent struct {
				Items []struct {
					ContentVersionID string `json:"content_version_id"`
				} `json:"items"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response, &listed))
	require.Len(t, listed.Result.StructuredContent.Items, 1, string(response))
	require.Equal(t, versionID, listed.Result.StructuredContent.Items[0].ContentVersionID)
	_, err = io.WriteString(input, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_vault_info","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
	require.NoError(t, err)
	response, err = reader.ReadBytes('\n')
	require.NoError(t, err)
	var info struct {
		Result struct {
			StructuredContent struct {
				VaultID         string `json:"vault_id"`
				ContentVersions int64  `json:"content_versions"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response, &info))
	require.Equal(t, vaultID, info.Result.StructuredContent.VaultID, string(response))
	require.Equal(t, int64(1), info.Result.StructuredContent.ContentVersions)
	_, err = io.WriteString(input, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_document","arguments":{"node_id":7,"content_version_id":"`+versionID+`"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
	require.NoError(t, err)
	response, err = reader.ReadBytes('\n')
	require.NoError(t, err)
	var document struct {
		Result struct {
			StructuredContent struct {
				NodeID           int64  `json:"node_id"`
				ContentVersionID string `json:"content_version_id"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response, &document))
	require.Equal(t, int64(7), document.Result.StructuredContent.NodeID, string(response))
	require.Equal(t, versionID, document.Result.StructuredContent.ContentVersionID, string(response))
	require.NoError(t, input.Close())
	require.NoError(t, command.Wait())
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
