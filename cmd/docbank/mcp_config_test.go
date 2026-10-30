package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPConfigStdio(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault with spaces")
	t.Setenv("DOCBANK_HOME", root)
	clientHome := t.TempDir()
	t.Setenv("HOME", clientHome)
	t.Setenv("USERPROFILE", clientHome)
	require.NoError(t, os.Mkdir(filepath.Join(clientHome, ".codex"), 0o700))
	clientFiles := []string{filepath.Join(clientHome, ".claude.json"), filepath.Join(clientHome, ".codex", "config.toml")}
	for _, path := range clientFiles {
		require.NoError(t, os.WriteFile(path, []byte("existing client configuration\n"), 0o600))
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, client := range []string{"claude-code", "codex"} {
		t.Run(client, func(t *testing.T) {
			out, err := runCLI(t, "mcp", "config", "--client", client,
				"--allow-processing", "--allow-package-writes", "--allow-photo-edits",
				"--allow-export-writes", "--allow-report-writes", "--allow-kata-writes")
			require.NoError(t, err)
			entry := decodeMCPConfig(t, client, out)
			assert.Equal(t, executable, entry["command"])
			assert.Equal(t, []any{"mcp", "--transport", "stdio", "--allow-processing", "--allow-package-writes", "--allow-photo-edits", "--allow-export-writes", "--allow-report-writes", "--allow-kata-writes"}, entry["args"])
			assert.Equal(t, map[string]any{"DOCBANK_HOME": root}, entry["env"])
			assert.True(t, strings.HasSuffix(out, "\n"))
		})
	}
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, path := range clientFiles {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "existing client configuration\n", string(data))
	}
}

func decodeMCPConfig(t *testing.T, client, out string) map[string]any {
	t.Helper()
	var document map[string]map[string]map[string]any
	if client == "codex" {
		require.NoError(t, toml.Unmarshal([]byte(out), &document))
		return document["mcp_servers"]["docbank"]
	}
	require.NoError(t, json.Unmarshal([]byte(out), &document))
	return document["mcpServers"]["docbank"]
}

func TestMCPConfigErrorsLeaveStdoutEmpty(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	t.Setenv("DOCBANK_MCP_HTTP_TOKEN", "")
	t.Setenv("DOCBANK_MCP_HTTP_TOKEN_FILE", "")
	for _, args := range [][]string{
		{}, {"--client", "claude"}, {"--client", "codex", "--transport", "invalid"},
		{"--client", "codex", "--listen", "127.0.0.1:7341"},
		{"--client", "codex", "--transport", "http"},
		{"--client", "codex", "--transport", "http", "--listen", "127.0.0.1:0"},
		{"--client", "codex", "--transport", "http", "--listen", "localhost:7341"},
		{"--client", "codex", "--transport", "http", "--listen", "127.0.0.1:7341"},
		{"--client", "codex", "--transport", "http", "--listen", "127.0.0.1:7341", "--allow-processing"},
	} {
		out, err := runCLI(t, append([]string{"mcp", "config"}, args...)...)
		require.Error(t, err)
		assert.Empty(t, out)
	}
}

func TestMCPConfigReportsOutputError(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	resetFlags(rootCmd)
	reader, writer := io.Pipe()
	require.NoError(t, reader.Close())
	defer func() { _ = writer.Close() }()
	rootCmd.SetOut(writer)
	defer rootCmd.SetOut(io.Discard)
	rootCmd.SetArgs([]string{"mcp", "config", "--client", "codex"})
	defer rootCmd.SetArgs(nil)
	require.ErrorIs(t, rootCmd.ExecuteContext(context.Background()), io.ErrClosedPipe)
}

func TestMCPConfigHTTPReferencesCredentialWithoutReadingIt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	t.Setenv("DOCBANK_MCP_HTTP_TOKEN", "")
	t.Setenv("DOCBANK_MCP_HTTP_TOKEN_FILE", "")
	t.Setenv("DOCBANK_TEST_SNIPPET_TOKEN", "")
	t.Setenv("DOCBANK_TEST_SNIPPET_TOKEN_FILE", filepath.Join(root, "missing-secret"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`[mcp.http]
credential_binding = 'credential:snippet'
[credential_bindings.snippet]
environment_variable = 'DOCBANK_TEST_SNIPPET_TOKEN'
`), 0o600))
	for _, client := range []string{"claude-code", "codex"} {
		for _, endpoint := range []struct{ listen, url string }{
			{"0.0.0.0:7341", "http://127.0.0.1:7341/mcp"},
			{"[::]:7341", "http://[::1]:7341/mcp"},
		} {
			out, err := runCLI(t, "mcp", "config", "--client", client, "--transport", "http", "--listen", endpoint.listen)
			require.NoError(t, err)
			entry := decodeMCPConfig(t, client, out)
			assert.Equal(t, endpoint.url, entry["url"])
			if client == "codex" {
				assert.Equal(t, "DOCBANK_TEST_SNIPPET_TOKEN", entry["bearer_token_env_var"])
			} else {
				assert.Equal(t, map[string]any{"Authorization": "Bearer ${DOCBANK_TEST_SNIPPET_TOKEN}"}, entry["headers"])
			}
		}
	}
	t.Setenv("DOCBANK_MCP_HTTP_TOKEN_FILE", filepath.Join(root, "unreadable-secret"))
	out, err := runCLI(t, "mcp", "config", "--client", "codex", "--transport", "http", "--listen", "127.0.0.1:7341")
	require.NoError(t, err)
	assert.Equal(t, "DOCBANK_MCP_HTTP_TOKEN", decodeMCPConfig(t, "codex", out)["bearer_token_env_var"])
}

func TestMCPConfigStartsSelectedCatalog(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	out, err := runCLI(t, "mcp", "config", "--client", "claude-code", "--allow-processing")
	require.NoError(t, err)
	var document struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &document))
	entry := document.Servers["docbank"]
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, entry.Command, "-test.run=^TestMCPCommandWriteFlagsSelectTools$")
	command.Env = append(os.Environ(), "DOCBANK_TEST_MCP_TOOL_CATALOG="+strings.Join(entry.Args, " "), "DOCBANK_HOME="+entry.Env["DOCBANK_HOME"])
	input, err := command.StdinPipe()
	require.NoError(t, err)
	defer func() { _ = input.Close() }()
	output, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() { _ = command.Process.Kill() })
	reader := bufio.NewReader(output)
	_, err = io.WriteString(input, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
	require.NoError(t, err)
	discovery, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, discovery, `"result"`)
	_, err = io.WriteString(input, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
	require.NoError(t, err)
	catalog, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, catalog, `"name":"start_processing"`)
	assert.Contains(t, catalog, `"name":"get_vault_info"`)
	assert.NotContains(t, catalog, `"name":"create_report"`)
	require.NoError(t, input.Close())
	require.NoError(t, command.Wait())
}
