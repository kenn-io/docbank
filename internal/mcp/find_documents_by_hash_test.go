package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPFindDocumentsByHash(t *testing.T) {
	f := newNativeExportFixture(t)
	selected := f.addFile(t, "current.txt", "synthetic shared bytes")
	old := f.addFile(t, "historical.txt", "synthetic shared bytes")
	node, err := f.catalog.NodeByID(t.Context(), old.NodeID)
	require.NoError(t, err)
	replacement := f.addFile(t, "replacement.txt", "replacement bytes")
	_, _, err = f.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, replacement.SHA256, replacement.Size, "text/plain")
	require.NoError(t, err)
	trash := f.addFile(t, "trashed.txt", "synthetic shared bytes")
	node, err = f.catalog.NodeByID(t.Context(), trash.NodeID)
	require.NoError(t, err)
	_, _, err = f.catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)

	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	out := exportCall(t, server, "find_documents_by_hash", map[string]any{"sha256": selected.SHA256})
	require.EqualValues(t, 1, out["total"])
	require.EqualValues(t, 50, out["limit"])
	items, ok := out["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, selected.NodeID, item["node_id"])
	require.Equal(t, selected.VersionID, item["content_version_id"])
	require.Equal(t, "/current.txt", item["path"])
	require.EqualValues(t, selected.Size, item["size"])
	require.Equal(t, selected.SHA256, item["blob_hash"])

	for _, args := range []map[string]any{
		{"sha256": strings.Repeat("0", 64)},
		{"sha256": selected.SHA256, "size": 0},
		{"sha256": selected.SHA256, "limit": 1, "offset": 1},
	} {
		out = exportCall(t, server, "find_documents_by_hash", args)
		require.Empty(t, out["items"])
	}
	other := f.addFile(t, "copy.txt", "synthetic shared bytes")
	out = exportCall(t, server, "find_documents_by_hash", map[string]any{
		"sha256": selected.SHA256, "size": selected.Size, "limit": 1, "offset": 1,
	})
	require.EqualValues(t, 2, out["total"])
	items, ok = out["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok = items[0].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, other.NodeID, item["node_id"])
}

func TestMCPFindDocumentsByHashAcceptsUploadedMediaType(t *testing.T) {
	f := newNativeExportFixture(t)
	const content = "synthetic document with a long MIME parameter"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	mediaType := "text/plain; title=" + strings.Repeat("a", 300)
	uploaded, err := f.connection.Upload(t.Context(), f.catalog.RootID(), "long-media-type.txt",
		mediaType, hash, int64(len(content)), strings.NewReader(content))
	require.NoError(t, err)
	require.Equal(t, mediaType, uploaded.Node.MimeType)

	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	out := exportCall(t, server, "find_documents_by_hash", map[string]any{"sha256": hash})
	require.EqualValues(t, 1, out["total"])
	items, ok := out["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, uploaded.Node.ID, item["node_id"])
	require.Equal(t, uploaded.Node.CurrentVersionID, item["content_version_id"])
	require.Equal(t, "/long-media-type.txt", item["path"])
	require.Equal(t, hash, item["blob_hash"])
	require.Equal(t, mediaType, item["media_type"])
}
