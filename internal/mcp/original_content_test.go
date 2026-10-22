package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"io"
	"maps"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPOriginalContent(t *testing.T) {
	f := newNativeExportFixture(t)
	original := "original\x00\xffbytes\n"
	selected := f.addFile(t, "reviewed.bin", original)
	node, err := f.catalog.NodeByID(t.Context(), selected.NodeID)
	require.NoError(t, err)
	hash, size, err := f.blobs.Write(strings.NewReader("replacement"))
	require.NoError(t, err)
	_, _, err = f.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, hash, size, "text/plain")
	require.NoError(t, err)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	args := map[string]any{"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID,
		"content_version_id": selected.VersionID, "blob_hash": selected.SHA256, "size": selected.Size,
		"max_bytes": 7}
	for _, transport := range []string{"stdio", "http"} {
		var received []byte
		for _, offset := range []int{0, 7, 14} {
			args["offset"] = offset
			result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
				"name": "read_document_content", "arguments": args,
			}))
			out := objectField(t, result, "structuredContent")
			require.Equal(t, f.catalog.VaultID(), out["vault_id"])
			require.Equal(t, selected.VersionID, out["content_version_id"])
			require.Equal(t, selected.SHA256, out["blob_hash"])
			require.EqualValues(t, 16, out["size"])
			require.EqualValues(t, offset, out["offset"])
			require.Equal(t, offset == 14, out["eof"])
			encoded, ok := out["data_base64"].(string)
			require.True(t, ok)
			data, err := base64.StdEncoding.DecodeString(encoded)
			require.NoError(t, err)
			received = append(received, data...)
			require.EqualValues(t, len(received), out["next_offset"])
		}
		require.Equal(t, []byte(original), received, "a newer document must not replace the selected version")
	}
}

func TestMCPOriginalContentSelection(t *testing.T) {
	f := newNativeExportFixture(t)
	selected := f.addFile(t, "selected.txt", "selected")
	other := f.addFile(t, "other.txt", "other")
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	args := map[string]any{"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID,
		"content_version_id": selected.VersionID, "blob_hash": selected.SHA256, "size": selected.Size}
	for field, value := range map[string]any{
		"vault_id": testVaultID, "node_id": other.NodeID, "content_version_id": other.VersionID,
		"blob_hash": strings.Repeat("a", 64), "size": 9, "offset": 9,
	} {
		t.Run(field, func(t *testing.T) {
			changed := maps.Clone(args)
			changed[field] = value
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "read_document_content", "arguments": changed,
			}))
			// An error must carry no original bytes.
			require.NotContains(t, string(raw), base64.StdEncoding.EncodeToString([]byte("selected")))
			require.NotZero(t, decodeWireError(t, raw).Code)
		})
	}
}

func TestMCPOriginalContentBoundsAndTrash(t *testing.T) {
	f := newNativeExportFixture(t)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	for _, content := range []string{"", strings.Repeat("z", maxOriginalChunk+1)} {
		selected := f.addFile(t, fmt.Sprintf("size-%d.bin", len(content)), content)
		args := map[string]any{"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID,
			"content_version_id": selected.VersionID, "blob_hash": selected.SHA256, "size": selected.Size,
			"max_bytes": maxOriginalChunk}
		raw := exportExchange(t, server, "http", "tools/call", map[string]any{"name": "read_document_content", "arguments": args})
		require.Less(t, len(raw), 1<<20)
		out := objectField(t, decodeResult(t, raw), "structuredContent")
		encoded, ok := out["data_base64"].(string)
		require.True(t, ok)
		data, err := base64.StdEncoding.DecodeString(encoded)
		require.NoError(t, err)
		require.Equal(t, []byte(content[:min(len(content), maxOriginalChunk)]), data)
		require.Equal(t, len(content) == 0, out["eof"])
		args["max_bytes"] = maxOriginalChunk + 1
		raw = exportExchange(t, server, "http", "tools/call", map[string]any{"name": "read_document_content", "arguments": args})
		require.EqualValues(t, -32602, decodeWireError(t, raw).Code)
		delete(args, "max_bytes")
		node, err := f.catalog.NodeByID(t.Context(), selected.NodeID)
		require.NoError(t, err)
		_, _, err = f.catalog.Trash(t.Context(), node.ID, node.Revision)
		require.NoError(t, err)
		raw = exportExchange(t, server, "http", "tools/call", map[string]any{"name": "read_document_content", "arguments": args})
		require.NotZero(t, decodeWireError(t, raw).Code)
	}
}

func TestOriginalChunkRejectsStreamMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, bytes, hash string
		size              int64
	}{
		{"short", "ab", "hash", 3}, {"long", "abcd", "hash", 3},
		{"hash", "abc", "other", 3}, {"size", "abc", "hash", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (originalContentInput{BlobHash: "hash", Size: 3, MaxBytes: 3}).readChunk(&daemonconn.ContentStream{
				ReadCloser: io.NopCloser(strings.NewReader(tc.bytes)), BlobHash: tc.hash, Size: tc.size,
			})
			require.Error(t, err)
		})
	}
}

func TestMCPOriginalContentKeepsProvenConnection(t *testing.T) {
	f := newNativeExportFixture(t)
	original := strings.Repeat("synthetic bytes\x00\xff", 1<<16)
	selected := f.addFile(t, "large.bin", original)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-original-key"
	daemon := api.NewServer(api.Deps{Store: f.catalog, Blobs: f.blobs,
		VaultRoot: os.Getenv("DOCBANK_HOME"), Cfg: cfg, ShutdownToken: "synthetic-proof"})
	t.Cleanup(daemon.Close)
	httpServer := httptest.NewServer(daemon.Handler())
	t.Cleanup(httpServer.Close)
	record := daemonconn.NewRecord(strings.TrimPrefix(httpServer.URL, "http://"), cfg.Server.APIKey, "synthetic-proof", "")
	_, err := daemonconn.RuntimeStore(os.Getenv("DOCBANK_HOME")).Write(record)
	require.NoError(t, err)
	connection, running, err := daemonconn.Running(t.Context())
	require.NoError(t, err)
	require.True(t, running)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return connection, nil
	}, func(c *daemonconn.Connection) error { return c.Close() })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	var received []byte
	for offset := 0; offset < len(original); offset += maxOriginalChunk {
		result := decodeResult(t, exportExchange(t, server, "http", "tools/call", map[string]any{
			"name": "read_document_content", "arguments": map[string]any{
				"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID,
				"content_version_id": selected.VersionID, "blob_hash": selected.SHA256,
				"size": selected.Size, "offset": offset, "max_bytes": maxOriginalChunk,
			},
		}))
		out := objectField(t, result, "structuredContent")
		encoded, ok := out["data_base64"].(string)
		require.True(t, ok)
		chunk, err := base64.StdEncoding.DecodeString(encoded)
		require.NoError(t, err)
		received = append(received, chunk...)
		require.EqualValues(t, len(received), out["next_offset"])
	}
	require.Equal(t, []byte(original), received)
}
