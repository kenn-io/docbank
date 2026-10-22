package api

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/kataevidence"
	"go.kenn.io/docbank/internal/store"
)

func TestKataContextOriginalCacheIsScopedToRequestAndSource(t *testing.T) {
	root := t.TempDir()
	s, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	blobsDir := filepath.Join(root, "blobs")
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	hash, size, err := blobs.Write(strings.NewReader("hello world"))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "notes.txt", hash, size, "text/plain")
	require.NoError(t, err)
	other, err := s.CreateFile(t.Context(), s.RootID(), "other.txt", hash, size, "text/plain")
	require.NoError(t, err)
	t.Setenv("DKE_TEST_KATA_KEY", "synthetic-key")
	deps := Deps{Store: s, Blobs: blobs, VaultRoot: root, Cfg: config.Default()}
	deps.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: "http://127.0.0.1:7777", APIKeyEnv: "DKE_TEST_KATA_KEY"}
	gate := NewOperationGate()
	service, err := kataService(deps, gate)
	require.NoError(t, err)
	selection := kataevidence.Selector{NodeID: node.ID, EndRune: 5}
	first, err := service.Archive.Prepare(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, "hello", first.Excerpt)
	// Removing the fixture blob makes a second physical read observable.
	path := filepath.Join(blobsDir, hash[:2], hash)
	require.NoError(t, os.Remove(path))
	ref := first.Reference
	ref.Docbank.StartRune, ref.Docbank.EndRune = 6, 11
	second, err := service.Archive.Resolve(t.Context(), ref, "world")
	require.NoError(t, err)
	require.Equal(t, "available", second.State)
	require.Equal(t, "world", second.Evidence.Excerpt)
	// A second node with identical bytes still owns a separate cache entry.
	selection.NodeID = other.ID
	_, err = service.Archive.Prepare(t.Context(), selection)
	require.ErrorIs(t, err, os.ErrNotExist)
	resolutionAfterSwitch, err := service.Archive.Resolve(t.Context(), ref, "world")
	require.NoError(t, err)
	require.Equal(t, "failed", resolutionAfterSwitch.State)
	// A later context request verifies the source again.
	next, err := kataService(deps, gate)
	require.NoError(t, err)
	resolution, err := next.Archive.Resolve(t.Context(), ref, "world")
	require.NoError(t, err)
	require.Equal(t, "failed", resolution.State)
	// Reload a successful original before checking the content-hash boundary.
	_, _, err = blobs.Write(strings.NewReader("hello world"))
	require.NoError(t, err)
	service, err = kataService(deps, gate)
	require.NoError(t, err)
	selection.NodeID = node.ID
	_, err = service.Archive.Prepare(t.Context(), selection)
	require.NoError(t, err)
	// A new content hash for the first node also requires a new read.
	newHash, newSize, err := blobs.Write(strings.NewReader("hello again"))
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, newHash, newSize, "text/plain")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(blobsDir, newHash[:2], newHash)))
	selection.NodeID = node.ID
	_, err = service.Archive.Prepare(t.Context(), selection)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestKataOriginalPreparationFailures(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	blobs, err := blob.New(store.NewPackCatalog(s), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	archive := kataArchive{deps: Deps{Store: s, Blobs: blobs}, gate: NewOperationGate(), originals: make(map[kataOriginalKey]kataOriginal)}
	for _, tc := range []struct {
		name     string
		mimeType string
		content  []byte
		start    int
		end      int
		quote    string
		detail   string
	}{
		{name: "yaml", mimeType: "application/yaml", content: []byte("hello"), end: 5},
		{name: "ndjson", mimeType: "application/x-ndjson", content: []byte("hello"), end: 5},
		{name: "json_charset", mimeType: "application/json; charset=utf-8", content: []byte("hello"), end: 5},
		{name: "too_large", mimeType: "text/plain", content: []byte(strings.Repeat("x", kataOriginalMaxBytes+1)), end: 1, detail: "document exceeds"},
		{name: "unsupported_media", mimeType: "application/pdf", content: []byte("synthetic PDF"), end: 1, detail: "retained text rendition"},
		{name: "octet_stream_text", mimeType: "application/octet-stream", content: []byte("hello"), end: 5, detail: "retained text rendition"},
		{name: "invalid_utf8", mimeType: "text/plain", content: []byte{0xff}, end: 1, detail: "not UTF-8 text"},
		{name: "end_beyond_text", mimeType: "text/plain", content: []byte("é🙂hello日"), start: 1, end: 9, quote: "🙂he", detail: "end_rune"},
		{name: "quote_mismatch", mimeType: "text/plain", content: []byte("hello"), end: 5, quote: "other", detail: "quote does not match the selected range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash, size, err := blobs.Write(bytes.NewReader(tc.content))
			require.NoError(t, err)
			node, err := s.CreateFile(t.Context(), s.RootID(), tc.name+".txt", hash, size, tc.mimeType)
			require.NoError(t, err)
			evidence, err := archive.Prepare(t.Context(), kataevidence.Selector{NodeID: node.ID, StartRune: tc.start, EndRune: tc.end, Quote: tc.quote})
			if tc.detail != "" {
				require.ErrorContains(t, err, tc.detail)
			} else {
				require.NoError(t, err)
				require.Equal(t, "hello", evidence.Excerpt)
			}
		})
	}
}
