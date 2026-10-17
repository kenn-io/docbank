package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func scopedHome(t *testing.T) string {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "DOCBANK_") {
			t.Setenv(name, "")
		}
	}

	root := t.TempDir()
	require.NoError(t, safefileio.EnsurePrivateDir(root))
	t.Setenv("DOCBANK_HOME", root)
	return root
}

func TestSearchWarnsOnUnusedInvalidDaemonConfig(t *testing.T) {
	root := scopedHome(t)
	startTestDaemon(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[web]\nsession_lifetime='1s'\n"), 0600))
	diagnostics, err := captureConfigStderr(t, func() error {
		_, err := runCLI(t, "search", "synthetic-absent-query")
		return err
	})
	require.NoError(t, err)
	require.Contains(t, diagnostics, "warning:")
	require.Contains(t, diagnostics, "[web]")
}

func captureConfigStderr(t *testing.T, run func() error) (string, error) {
	t.Helper()
	read, write, err := os.Pipe()
	require.NoError(t, err)
	previous := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = previous; _ = write.Close(); _ = read.Close() }()
	runErr := run()
	os.Stderr = previous
	require.NoError(t, write.Close())
	diagnostics, err := io.ReadAll(read)
	require.NoError(t, err)
	return string(diagnostics), runErr
}

func TestEditWarnsOnceWithoutCachingConfigParsing(t *testing.T) {
	root := scopedHome(t)
	startTestDaemon(t, root)
	source := writeSourceFile(t, "notes.txt", "original")
	_, err := runCLI(t, "add", source, "--dest", "/")
	require.NoError(t, err)
	setEditHelper(t, "updated")
	path := filepath.Join(root, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[web]\nsession_lifetime='1s'\n"), 0600))
	diagnostics, err := captureConfigStderr(t, func() error {
		_, err := runCLI(t, "edit", "/notes.txt")
		return err
	})
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(diagnostics, "warning:"), diagnostics)
	require.NoError(t, os.WriteFile(path, []byte("[web\n"), 0600))
	_, err = runCLI(t, "search", "synthetic-query")
	require.ErrorContains(t, err, "loading")
}

func TestDaemonAndAutoStartRejectInvalidConfigPrecisely(t *testing.T) {
	root := scopedHome(t)
	bin := buildDocbank(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[web]\nsession_lifetime='1s'\n"), 0600))
	_, err := runCLI(t, "daemon", "run")
	require.ErrorContains(t, err, "[web]")
	child := exec.CommandContext(t.Context(), bin, "search", "synthetic-query")
	output, err := child.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "daemon config invalid: [web]")
}

func TestConfigSyntaxAndUnknownKeysFailCommandBoundaries(t *testing.T) {
	root := scopedHome(t)
	startTestDaemon(t, root)
	for _, body := range []string{"[web\n", "[web]\nunknown_setting=true\n"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(body), 0600))
		_, err := runCLI(t, "search", "synthetic-query")
		require.ErrorContains(t, err, "loading")
		_, _, err = loadMCPHTTPConfig(root)
		require.ErrorContains(t, err, "loading")
		fresh := scopedHome(t)
		require.NoError(t, os.WriteFile(filepath.Join(fresh, "config.toml"), []byte(body), 0600))
		_, err = runCLI(t, "daemon", "run")
		require.ErrorContains(t, err, "loading")
		t.Setenv("DOCBANK_HOME", root)
	}
}

func TestMCPHTTPLoadIgnoresUnrelatedDaemonConfig(t *testing.T) {
	root := scopedHome(t)
	t.Setenv("SCV_MCP_TOKEN", "synthetic-mcp-token")
	t.Setenv("DOCBANK_API_PORT", "invalid")
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`[web]
session_lifetime='1s'
[[watch]]
name='relative'
source='relative/path'
[mcp.http]
credential_binding='credential:inbound'
[credential_bindings.inbound]
environment_variable='SCV_MCP_TOKEN'
`), 0600))
	_, token, err := loadMCPHTTPConfig(root)
	require.NoError(t, err)
	require.Equal(t, "synthetic-mcp-token", token)
}
