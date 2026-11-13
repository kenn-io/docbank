package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentsetup"
)

func TestAgentSetupFeatureStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	stub := filepath.Join(dir, "codex")
	for _, tc := range []struct{ output, state, detail string }{
		{"mcp_2026_07_28 under development true", "ok", "under-development"},
		{"mcp_2026_07_28 under development false", "failed", "support is off"},
		{"other under development true", "failed", "upgrade Codex"},
		{"mcp_2026_07_28 under development perhaps", "unknown", "parse"},
	} {
		t.Run(tc.state+tc.detail, func(t *testing.T) {
			require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' '"+tc.output+"'\n"), 0o700))
			status, err := codexFeatureItem().Check(t.Context())
			require.NoError(t, err)
			assert.Equal(t, agentsetup.State(tc.state), status.State)
			assert.Contains(t, status.Reason+status.NextStep, tc.detail)
		})
	}
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o700))
	status, err := codexFeatureItem().Check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateUnknown, status.State)
}

func TestAgentSetupCheckAndPrint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex-home"))
	t.Setenv("DOCBANK_HOME", filepath.Join(dir, "vault"))
	t.Setenv("PATH", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\necho 'mcp_2026_07_28 under development false'\n"), 0o700))
	resetFlags(rootCmd)
	var out, stderr bytes.Buffer
	code := runProcess([]string{"agent", "setup", "--agent", "codex", "--check", "--json"}, &out, &stderr)
	assert.Equal(t, 1, code)
	assert.Contains(t, out.String(), `"state":"failed"`)
	assert.Contains(t, out.String(), `"state":"missing"`)
	assert.Contains(t, out.String(), codexEnable)
	assert.Empty(t, stderr.String())
	resetFlags(rootCmd)
	out.Reset()
	stderr.Reset()
	code = runProcess([]string{"agent", "setup", "--agent", "codex", "--print"}, &out, &stderr)
	require.Equal(t, 0, code, stderr.String())
	assert.True(t, strings.HasPrefix(out.String(), codexEnable+"\n"))
}

func TestAgentSetupCodexServerDiagnostics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	for _, tc := range []struct {
		name, reason, want string
	}{
		{"version mismatch", "JSON-RPC error: -32022: Docbank supports only MCP protocol version 2026-07-28; use server/discover", "Codex MCP protocol version mismatch: " + codexEnable},
		{"other failure", "synthetic server failure", "synthetic server failure: Correct the server error and retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("DOCBANK_HOME", filepath.Join(dir, "vault"))
			t.Setenv("PATH", dir)
			t.Setenv("STUB_ERROR", tc.reason)
			stub := `#!/bin/sh
if [ "$1 $2" = "features list" ]; then
  echo 'mcp_2026_07_28 under development false'
elif [ "$1 $2" = "app-server --stdio" ]; then
  read -r line
  echo '{"id":1,"result":{}}'
  read -r line
  read -r line
  printf '{"id":2,"result":{"data":[{"name":"docbank","toolsError":"%s"}]}}\n' "$STUB_ERROR"
  while read -r line; do :; done
fi
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "codex"), []byte(stub), 0o700))
			manual, err := runCLI(t, "mcp", "config", "--client", "codex")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(manual), 0o600))
			out, err := runCLI(t, "agent", "setup", "--agent", "codex", "--check")
			require.ErrorIs(t, err, agentsetup.ErrNotReady)
			assert.Contains(t, out, tc.want)
			if tc.name == "version mismatch" {
				assert.NotContains(t, out, "server/discover")
			}
		})
	}
}

func TestAgentSetupFeatureEnable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	state := filepath.Join(dir, "state")
	t.Setenv("FEATURE_STATE", state)
	stub := `#!/bin/sh
if [ "$1 $2" = "features list" ]; then
  if [ -f "$FEATURE_STATE" ]; then echo 'mcp_2026_07_28 under development true'; else echo 'mcp_2026_07_28 under development false'; fi
elif [ "$1 $2 $3" = "features enable mcp_2026_07_28" ]; then
  echo enabled >> "$FEATURE_STATE"
else
  exit 1
fi
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex"), []byte(stub), 0o700))
	item := codexFeatureItem()
	require.NoError(t, item.Install(t.Context()))
	require.NoError(t, item.Install(t.Context()))
	contents, err := os.ReadFile(state)
	require.NoError(t, err)
	assert.Equal(t, "enabled\n", string(contents))
	status, err := item.Check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateOK, status.State)

	require.NoError(t, os.Remove(state))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nif [ \"$2\" = list ]; then echo 'mcp_2026_07_28 under development false'; else echo 'enable refused' >&2; exit 1; fi\n"), 0o700))
	err = item.Install(t.Context())
	require.ErrorContains(t, err, "enable refused")
}

func TestAgentSetupResetsManualWriteArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	for _, tc := range []struct{ agent, client, config string }{
		{"codex", "codex", "config.toml"},
		{"claude", "claude-code", ".claude.json"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("USERPROFILE", dir)
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("DOCBANK_HOME", filepath.Join(dir, "vault"))
			t.Setenv("PATH", dir)
			stub := "#!/bin/sh\nif [ \"$1 $2\" = 'features list' ]; then echo 'mcp_2026_07_28 under development true'; else exit 1; fi\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, tc.agent), []byte(stub), 0o700))
			manual, err := runCLI(t, "mcp", "config", "--client", tc.client, "--allow-processing")
			require.NoError(t, err)
			path := filepath.Join(dir, tc.config)
			require.NoError(t, os.WriteFile(path, []byte(manual), 0o600))
			out, _ := runCLI(t, "agent", "setup", "--agent", tc.agent, "--check")
			assert.Contains(t, out, "outdated")
			if tc.agent == "claude" {
				assert.NotContains(t, out, codexFeature)
			}
			unchanged, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, manual, string(unchanged))
			_, _ = runCLI(t, "agent", "setup", "--agent", tc.agent)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, []any{"mcp", "--transport", "stdio"}, decodeMCPConfig(t, tc.client, string(data))["args"])
			_, _ = runCLI(t, "agent", "setup", "--agent", tc.agent)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, data, after)
		})
	}
}
