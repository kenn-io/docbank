package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDaemonAutoStartReportsInvalidConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	bin := buildDocbank(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[web]\nsession_lifetime='1s'\n"), 0600))
	child := exec.CommandContext(t.Context(), bin, "search", "synthetic-query")
	output, err := child.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "daemon config invalid: [web] session_lifetime")
}
