package mcp

import (
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

type contentCountingWriter struct {
	http.ResponseWriter

	bytes *atomic.Int64
}

func (w contentCountingWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.bytes.Add(int64(n))
	return n, err
}

func TestMCPOriginalContentTransfersOnlyWindow(t *testing.T) {
	for _, storage := range []string{"raw", "zstd", "packed"} {
		t.Run(storage, func(t *testing.T) {
			f := newNativeExportFixture(t)
			if storage == "zstd" {
				writer, err := blob.NewWithOptions(store.NewPackCatalog(f.catalog), filepath.Join(os.Getenv("DOCBANK_HOME"), "blobs"), blob.ManagedOptions())
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, writer.Close()) })
				f.blobs = writer
			}
			original := strings.Repeat("synthetic window bytes", 1<<16)
			receipt, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader(original[:1<<20]))
			require.NoError(t, err)
			encoding, err := receipt.EncodingName()
			require.NoError(t, err)
			if storage == "zstd" {
				require.Equal(t, "zstd", encoding)
			} else {
				require.Equal(t, "raw", encoding)
			}
			selected, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "window.bin", receipt.Hash, receipt.Size, "text/plain", store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, Created: receipt.Created, MD5: receipt.MD5})
			require.NoError(t, err)
			if storage == "packed" {
				result, err := f.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
				require.NoError(t, err)
				require.Equal(t, 1, result.BlobsPacked)
			}
			cfg := config.Default()
			cfg.Server.APIKey = "synthetic-export-key"
			daemon := api.NewServer(api.Deps{Store: f.catalog, Blobs: f.blobs, Cfg: cfg, VaultRoot: os.Getenv("DOCBANK_HOME")})
			t.Cleanup(daemon.Close)
			var transferred atomic.Int64
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/content") {
					w = contentCountingWriter{w, &transferred}
				}
				daemon.Handler().ServeHTTP(w, r)
			}))
			t.Cleanup(ts.Close)
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, ts.URL))
			result := exportCall(t, server, "read_document_content", map[string]any{
				"vault_id": f.catalog.VaultID(), "node_id": selected.ID,
				"content_version_id": selected.CurrentVersionID, "blob_hash": selected.BlobHash,
				"size": selected.Size, "offset": selected.Size - (64 << 10), "max_bytes": 64 << 10,
			})
			encoded, ok := result["data_base64"].(string)
			require.True(t, ok)
			data, err := base64.StdEncoding.DecodeString(encoded)
			require.NoError(t, err)
			require.Equal(t, []byte(original[(1<<20)-(64<<10):1<<20]), data)
			require.Less(t, transferred.Load(), int64(2*(64<<10)))
		})
	}
}

func TestMCPOriginalContent(t *testing.T) {
	f := newNativeExportFixture(t)
	original := "original\x00\xffbytes\n"
	selected := f.addFile(t, "reviewed.bin", original)
	node, err := f.catalog.NodeByID(t.Context(), selected.NodeID)
	require.NoError(t, err)
	hash, size, err := f.blobs.WriteContext(t.Context(), strings.NewReader("replacement"))
	require.NoError(t, err)
	_, _, err = f.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, hash, size, "text/plain")
	require.NoError(t, err)
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/info" {
			http.Error(w, "unreadable unrelated blob shard", http.StatusInternalServerError)
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(wrapped.Close)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, wrapped.URL))
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
	server := NewServerWithConnection(f.provenConnection(t), ServerOptions{})
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
