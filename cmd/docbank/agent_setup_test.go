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
	require.NoError(t, os.Remove(stub))
	status, err = codexFeatureItem().Check(t.Context())
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
	assert.Contains(t, out.String(), codexEnable)
	assert.Empty(t, stderr.String())
	_, err := os.Stat(filepath.Join(dir, "codex-home", "config.toml"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	resetFlags(rootCmd)
	out.Reset()
	stderr.Reset()
	code = runProcess([]string{"agent", "setup", "--agent", "codex", "--print", "--allow-processing"}, &out, &stderr)
	require.Equal(t, 0, code, stderr.String())
	assert.True(t, strings.HasPrefix(out.String(), codexEnable+"\n"))
	assert.Contains(t, out.String(), "--allow-processing")
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

func TestAgentSetupWritesCodexEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("DOCBANK_HOME", filepath.Join(dir, "vault"))
	t.Setenv("PATH", dir)
	config := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(config, []byte("[mcp_servers.other]\ncommand = 'other'\n"), 0o600))
	t.Setenv("FEATURE_STATE", filepath.Join(dir, "state"))
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
	out, err := runCLI(t, "agent", "setup", "--agent", "codex", "--allow-processing")
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	assert.Contains(t, out, "on; Codex shows an under-development feature notice")
	state, err := os.ReadFile(filepath.Join(dir, "state"))
	require.NoError(t, err)
	assert.Equal(t, "enabled\n", string(state))
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	entry := decodeMCPConfig(t, "codex", string(data))
	assert.Equal(t, []any{"mcp", "--transport", "stdio", "--allow-processing"}, entry["args"])
	assert.Equal(t, "2026-07-28", entry["env"].(map[string]any)["CODEX_MCP_PROTOCOL_VERSION"])
	assert.Contains(t, string(data), "[mcp_servers.other]")
	out, err = runCLI(t, "agent", "setup", "--agent", "codex")
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	assert.Contains(t, out, "on; Codex shows an under-development feature notice")
	state, err = os.ReadFile(filepath.Join(dir, "state"))
	require.NoError(t, err)
	assert.Equal(t, "enabled\n", string(state))
	data, err = os.ReadFile(config)
	require.NoError(t, err)
	assert.Equal(t, []any{"mcp", "--transport", "stdio"}, decodeMCPConfig(t, "codex", string(data))["args"])
}

func TestAgentSetupClaudeEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("DOCBANK_HOME", filepath.Join(dir, "vault"))
	t.Setenv("PATH", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o700))
	out, err := runCLI(t, "agent", "setup", "--agent", "claude", "--json")
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	assert.Contains(t, out, `"piece":"mcp docbank"`)
	assert.NotContains(t, out, codexFeature)
	data, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	require.NoError(t, err)
	entry := decodeMCPConfig(t, "claude-code", string(data))
	assert.Equal(t, []any{"mcp", "--transport", "stdio"}, entry["args"])
	assert.Equal(t, filepath.Join(dir, "vault"), entry["env"].(map[string]any)["DOCBANK_HOME"])
}
