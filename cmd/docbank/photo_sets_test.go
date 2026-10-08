package main

import (
	"encoding/json/v2"
	"image/color"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func runAlbumCLI[T any](t *testing.T, args ...string) T {
	t.Helper()
	out, err := runCLI(t, append([]string{"photos", "albums"}, args...)...)
	require.NoError(t, err, out)
	var value T
	require.NoError(t, json.Unmarshal([]byte(out), &value), out)
	return value
}

func TestPhotoAlbumCLIWorkflow(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "album-image.jpeg", string(mediatest.JPEG(2, 2, color.White)))
	_, err := runCLI(t, "add", source, "--dest", "/inbox")
	require.NoError(t, err)
	c, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := c.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/inbox/album-image.jpeg"}})
	require.NoError(t, err)
	asset, err := c.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)

	album := runAlbumCLI[api.PhotoAlbum](t, "create", "Holiday")
	assert.Equal(t, int64(1), album.Revision)
	for _, args := range [][]string{
		{"add", album.ID},
		{"add", album.ID, asset.ID, "--query", `{}`},
		{"add", album.ID, "--query", `{"unknown":1}`},
		{"members", album.ID, "--sort", "name"},
	} {
		_, err := runCLI(t, append([]string{"photos", "albums"}, args...)...)
		require.Error(t, err, args)
		assert.Equal(t, exitUsage, commandExitCode(err, true), args)
	}

	album = runAlbumCLI[api.PhotoAlbum](t, "add", album.ID, "--query", `{"filters":{"kinds":["photo"]}}`)
	assert.Equal(t, int64(2), album.Revision)
	album = runAlbumCLI[api.PhotoAlbum](t, "cover", album.ID, asset.ID)
	require.NotNil(t, album.CoverAssetID)
	assert.Equal(t, asset.ID, *album.CoverAssetID)
	album = runAlbumCLI[api.PhotoAlbum](t, "cover", album.ID)
	assert.Nil(t, album.CoverAssetID)
	album = runAlbumCLI[api.PhotoAlbum](t, "star", album.ID)
	assert.True(t, album.Starred)
	album = runAlbumCLI[api.PhotoAlbum](t, "star", album.ID, "--starred=false")
	assert.False(t, album.Starred)
	_, err = runCLI(t, "photos", "albums", "rename", album.ID, "Stale", "--revision", strconv.FormatInt(album.Revision-1, 10))
	require.Error(t, err)
	assert.Equal(t, exitStale, commandExitCode(err, true))

	page := runAlbumCLI[api.PhotoBrowsePage](t, "members", album.ID)
	assert.Equal(t, int64(1), page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, asset.ID, page.Items[0].AssetID)
	summary := runAlbumCLI[api.PhotoAlbumSummary](t, "show", album.ID)
	assert.Equal(t, int64(1), summary.MemberCount)

	album = runAlbumCLI[api.PhotoAlbum](t, "remove", album.ID, asset.ID)
	summary = runAlbumCLI[api.PhotoAlbumSummary](t, "show", album.ID)
	assert.Zero(t, summary.MemberCount)
	deleted := runAlbumCLI[api.PhotoAlbum](t, "delete", album.ID)
	assert.NotNil(t, deleted.DeletedAt)
	assert.Empty(t, runAlbumCLI[[]api.PhotoAlbumSummary](t, "list"))
}
