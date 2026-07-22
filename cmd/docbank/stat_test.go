package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestStatCLIInspectsLiveAndTrashedNodes(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "record.txt", "inspectable content")
	_, err := runCLI(t, "add", source, "--dest", "/archive")
	require.NoError(t, err)

	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	node, err := c.Stat(context.Background(), "/archive/record.txt")
	require.NoError(t, err)
	selector := formatNodeSelector(node.ID)

	out, err := runCLI(t, "stat", "/archive/record.txt")
	require.NoError(t, err, out)
	assert.Contains(t, out, "selector:  "+selector)
	assert.Contains(t, out, "state:     live")
	assert.Contains(t, out, `path:      "/archive/record.txt"`)
	assert.Contains(t, out, "kind:      file")
	assert.Contains(t, out, "version:   "+node.CurrentVersionID)
	assert.Contains(t, out, "sha256:    "+node.BlobHash)
	assert.Contains(t, out, "mime:      \"text/plain; charset=utf-8\"")

	out, err = runCLI(t, "stat", selector, "--json")
	require.NoError(t, err, out)
	var got api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, node, got)

	_, err = runCLI(t, "rm", selector)
	require.NoError(t, err)
	out, err = runCLI(t, "stat", selector)
	require.NoError(t, err, out)
	assert.Contains(t, out, "state:     trashed")
	assert.NotContains(t, out, "path:")
	assert.Contains(t, out, "trashed:")

	out, err = runCLI(t, "stat", selector, "--json")
	require.NoError(t, err, out)
	got = api.Node{}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.NotEmpty(t, got.TrashedAt)
	assert.Empty(t, got.Path)
}

func TestStatCLIValidatesSelectorBeforeDaemonStartup(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	_, err := runCLI(t, "stat", "relative/path")
	require.ErrorContains(t, err, "absolute virtual path")
}
