package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestVersionsShowIncludesAuxiliaryMD5(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "versioned.txt", "versioned content")
	_, err := runCLI(t, "add", source, "--dest", "/archive")
	require.NoError(t, err)

	c, err := daemonconn.Ensure(context.Background())
	require.NoError(t, err)
	node, err := c.API().ResolvePath(context.Background(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/archive/versioned.txt"}})

	require.NoError(t, err)
	require.NotEmpty(t, node.MD5)

	out, err := runCLI(t, "versions", "show", node.CurrentVersionID)
	require.NoError(t, err, out)
	assert.Contains(t, out, "Blob:           "+node.BlobHash)
	assert.Contains(t, out, "MD5:            "+node.MD5)
}
