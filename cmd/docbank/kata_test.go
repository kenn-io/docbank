package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKataCLIOptInAndUsage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	_, err := runCLI(t, "kata", "cite", "id:2", "--start", "0", "--end", "5", "--title", "Follow up", "--idempotency-key", "test")
	require.ErrorContains(t, err, "kata integration is disabled")
	_, err = runCLI(t, "kata", "context", "abcd")
	require.ErrorContains(t, err, "kata integration is disabled")
	_, err = os.Stat(filepath.Join(root, "docbank.db"))
	require.True(t, os.IsNotExist(err), "disabled integration must not create a vault")
	_, err = runCLI(t, "kata", "cite", "id:2", "--end", "1001", "--title", "Follow up", "--idempotency-key", "test")
	require.ErrorContains(t, err, "range")
}

func TestKataCLILeavesEnvironmentCredentialToDaemon(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	t.Setenv("DKE_DAEMON_ONLY_KEY", "")
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[integrations.kata]\nenabled=true\nendpoint='http://127.0.0.1:7777'\napi_key_env='DKE_DAEMON_ONLY_KEY'\n"), 0600))
	require.NoError(t, checkKataConfig(), "the daemon may have an environment credential that its CLI does not")
}
