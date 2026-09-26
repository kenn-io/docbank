package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestAgentSessionCLIFileAcrossInvocationsAndRevocation(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "synthetic-agent-source.txt", "synthetic scoped source")
	_, err := runCLI(t, "add", source, "--dest", "/")
	require.NoError(t, err)
	master, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := master.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{
		Query: &apiclient.ResolvePathQuery{Path: "/synthetic-agent-source.txt"},
	})
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "agent-session.json")
	output, err := runCLI(t, "agent-session", "issue", "--source-id", node.CurrentVersionID,
		"--ttl", "5m", "--output", file)
	require.NoError(t, err, output)
	id := regexp.MustCompile(`session ([0-9a-f]{32})`).FindStringSubmatch(output)
	require.Len(t, id, 2, output)
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	var binding daemonconn.AgentSessionFile
	require.NoError(t, json.Unmarshal(raw, &binding, json.RejectUnknownMembers(true)))
	require.Equal(t, 1, binding.Version)
	require.NotEmpty(t, binding.Token)
	require.NotContains(t, output, binding.Token)
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(file)
		require.NoError(t, statErr)
		require.Zero(t, info.Mode().Perm()&0o077)
	}
	t.Setenv(daemonconn.AgentSessionFileEnv, file)
	scoped, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	_, err = scoped.API().ReadCapabilities(t.Context())
	require.NoError(t, err)
	_, err = runCLI(t, "agent-session", "revoke", id[1])
	require.Error(t, err, "an explicit scoped binding must not reacquire the master key")
	require.NoError(t, os.Unsetenv(daemonconn.AgentSessionFileEnv))
	_, err = runCLI(t, "agent-session", "revoke", id[1])
	require.NoError(t, err)
	_, err = scoped.API().ReadCapabilities(t.Context())
	require.Error(t, err, "revocation must reject the previously attached client")
}
