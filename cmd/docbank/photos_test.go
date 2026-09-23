package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPhotosCLIEnrollsAddedImage(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "synthetic-image.jpg", "synthetic image bytes")
	_, err := runCLI(t, "add", source, "--dest", "/inbox")
	require.NoError(t, err)
	c, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := c.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/inbox/synthetic-image.jpg"}})
	require.NoError(t, err)
	assetFromNode, err := c.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	out, err := runCLI(t, "photos", "assets", "inspect", assetFromNode.ID)
	require.NoError(t, err)
	var asset api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(out), &asset))
	assert.Equal(t, int64(1), asset.Revision)
	require.Len(t, asset.Files, 1)
	assert.Equal(t, node.ID, asset.Files[0].NodeID)
}

func TestPhotosCLIWorkflow(t *testing.T) {
	paths := [][]string{
		{"photos", "assets", "create"},
		{"photos", "assets", "inspect"},
		{"photos", "assets", "attach"},
		{"photos", "assets", "detach"},
		{"photos", "assets", "exclude"},
		{"photos", "assets", "promote"},
		{"photos", "assets", "display"},
		{"photos", "settings", "show"},
		{"photos", "settings", "set"},
		{"photos", "settings", "reset"},
	}
	for _, path := range paths {
		command, _, err := rootCmd.Find(path)
		require.NoError(t, err, path)
		assert.NotNil(t, command, path)
	}
	for _, path := range [][]string{
		{"photos", "assets", "attach"}, {"photos", "assets", "detach"},
		{"photos", "assets", "exclude"}, {"photos", "assets", "display"},
		{"photos", "settings", "set"}, {"photos", "settings", "reset"},
	} {
		command, _, err := rootCmd.Find(path)
		require.NoError(t, err)
		assert.NotNil(t, command.Flags().Lookup("revision"), path)
	}
}
