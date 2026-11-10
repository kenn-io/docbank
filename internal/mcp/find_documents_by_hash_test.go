package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
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
	_, err := f.connection.Upload(t.Context(), f.catalog.RootID(), "long-media-type.txt",
		mediaType, hash, int64(len(content)), strings.NewReader(content))
	require.NoError(t, err)

	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	out := exportCall(t, server, "find_documents_by_hash", map[string]any{"sha256": hash})
	items, ok := out["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, mediaType, item["media_type"])
}

func TestMCPFindDocumentsByHashPagesLongPaths(t *testing.T) {
	f := newNativeExportFixture(t)
	parentID := f.catalog.RootID()
	// These are catalog paths, not filesystem paths: every segment is valid.
	for range 48 {
		folder, err := f.catalog.Mkdir(t.Context(), parentID, strings.Repeat("p", 250))
		require.NoError(t, err)
		parentID = folder.ID
	}
	hash, size, err := f.blobs.WriteContext(t.Context(), strings.NewReader("synthetic shared long-path content"))
	require.NoError(t, err)
	expected := make(map[int64]bool)
	for i := range 50 {
		node, err := f.catalog.CreateFile(t.Context(), parentID, fmt.Sprintf("file-%02d.txt", i), hash, size, "text/plain")
		require.NoError(t, err)
		expected[node.ID] = true
	}
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	seen := make(map[int64]bool)
	offset := 0
	for {
		result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": "find_documents_by_hash", "arguments": map[string]any{"sha256": hash, "offset": offset},
		})))
		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), maxToolResponseBytes)
		out := objectField(t, result, "structuredContent")
		require.EqualValues(t, 50, out["total"])
		require.EqualValues(t, offset, out["offset"])
		items, ok := out["items"].([]any)
		require.True(t, ok)
		require.NotEmpty(t, items)
		if offset == 0 {
			require.Less(t, len(items), 50, "the oversized first page must shrink")
		}
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			require.True(t, ok)
			nodeID, ok := item["node_id"].(float64)
			require.True(t, ok)
			id := int64(nodeID)
			require.True(t, expected[id])
			require.False(t, seen[id], "continuation must not repeat rows")
			seen[id] = true
		}
		next, more := out["next_offset"]
		if !more {
			break
		}
		require.EqualValues(t, offset+len(items), next)
		offset += len(items)
		require.Less(t, offset, 50)
	}
	require.Equal(t, expected, seen)
}
