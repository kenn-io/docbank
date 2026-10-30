package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPConfigStdio(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root = filepath.Join(root, "vault with spaces")
	t.Setenv("DOCBANK_HOME", root)
	executable, err := exec.LookPath(os.Args[0])
	require.NoError(t, err)
	executable, err = filepath.Abs(executable)
	require.NoError(t, err)
	flags := []string{"--allow-processing", "--allow-package-writes", "--allow-photo-edits", "--allow-export-writes", "--allow-report-writes", "--allow-kata-writes"}
	want := []any{"mcp", "--transport", "stdio", "--allow-export-writes", "--allow-kata-writes", "--allow-package-writes", "--allow-photo-edits", "--allow-processing", "--allow-report-writes"}
	for _, test := range []struct {
		client string
		flags  []string
		want   []any
	}{
		{"claude-code", flags, want}, {"codex", flags, want},
		{"claude-code", []string{"--allow-processing", "--allow-report-writes=false"}, []any{"mcp", "--transport", "stdio", "--allow-processing"}},
	} {
		client := test.client
		t.Run(client, func(t *testing.T) {
			out, err := runCLI(t, append([]string{"mcp", "config", "--client", client}, test.flags...)...)
			require.NoError(t, err)
			entry := decodeMCPConfig(t, client, out)
			assert.Equal(t, executable, entry["command"])
			assert.Equal(t, test.want, entry["args"])
			env := map[string]any{"DOCBANK_HOME": root}
			if client == "codex" {
				env["CODEX_MCP_PROTOCOL_VERSION"] = "2026-07-28"
			}
			assert.Equal(t, env, entry["env"])
		})
	}
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMCPConfigLaunchPath(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, mode := range []string{"symlink", "current directory"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			launch := filepath.Join(root, "docbank.exe")
			if mode == "symlink" {
				if err := os.Symlink(executable, launch); err != nil {
					t.Skipf("symlink creation unavailable: %v", err)
				}
			} else {
				t.Chdir(root)
				t.Setenv("PATH", ".")
				require.NoError(t, os.WriteFile(launch, nil, 0o700))
			}
			t.Setenv("DOCBANK_HOME", root)
			original := os.Args[0]
			os.Args[0] = launch
			if mode == "current directory" {
				os.Args[0] = "docbank.exe"
			}
			t.Cleanup(func() { os.Args[0] = original })
			out, err := runCLI(t, "mcp", "config", "--client", "claude-code")
			require.NoError(t, err)
			assert.Equal(t, launch, decodeMCPConfig(t, "claude-code", out)["command"])
		})
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
		{"--client", "claude"}, {"--client", "codex", "--transport", "invalid"},
		{"--client", "codex", "--transport", "http", "--listen", "127.0.0.1:0"},
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
	out, err := runCLI(t, "mcp", "config", "--client", "codex", "--transport", "http", "--listen", "127.0.0.1:7341")
	require.NoError(t, err)
	assert.Equal(t, "DOCBANK_MCP_HTTP_TOKEN", decodeMCPConfig(t, "codex", out)["bearer_token_env_var"])
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`[mcp.http]
credential_binding = 'credential:snippet'
[credential_bindings.snippet]
environment_variable = 'DOCBANK_TEST_SNIPPET_TOKEN'
`), 0o600))
	for _, endpoint := range []struct{ client, listen, url string }{
		{"codex", "0.0.0.0:7341", "http://127.0.0.1:7341/mcp"},
		{"codex", "[::]:7341", "http://[::1]:7341/mcp"},
		{"claude-code", "127.0.0.1:7341", "http://127.0.0.1:7341/mcp"},
	} {
		out, err := runCLI(t, "mcp", "config", "--client", endpoint.client, "--transport", "http", "--listen", endpoint.listen)
		require.NoError(t, err)
		entry := decodeMCPConfig(t, endpoint.client, out)
		assert.Equal(t, endpoint.url, entry["url"])
		if endpoint.client == "codex" {
			assert.Equal(t, "DOCBANK_TEST_SNIPPET_TOKEN", entry["bearer_token_env_var"])
		} else {
			assert.Equal(t, map[string]any{"Authorization": "Bearer ${DOCBANK_TEST_SNIPPET_TOKEN}"}, entry["headers"])
		}
	}
}
