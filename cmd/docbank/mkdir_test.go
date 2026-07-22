package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestMkdirCLICreatesExactDirectory(t *testing.T) {
	_ = setupVaultHome(t)

	out, err := runCLI(t, "mkdir", "/Projects")
	require.NoError(t, err)
	assert.Contains(t, out, "created id:")
	assert.Contains(t, out, `"/Projects"`)

	out, err = runCLI(t, "mkdir", "/Projects/2026/", "--json")
	require.NoError(t, err)
	var node api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &node))
	assert.Equal(t, "/Projects/2026", node.Path)
	assert.Equal(t, "2026", node.Name)
	assert.Equal(t, "dir", node.Kind)

	_, err = runCLI(t, "mkdir", "/Projects/2026")
	require.ErrorContains(t, err, "name already exists")
	_, err = runCLI(t, "mkdir", "/Missing/Child")
	require.ErrorContains(t, err, "not found")
}

func TestMkdirCLIValidatesPathBeforeDaemonStartup(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	for _, path := range []string{"relative", "/", "/valid/../invalid"} {
		_, err := runCLI(t, "mkdir", path)
		require.Error(t, err, path)
	}
}
