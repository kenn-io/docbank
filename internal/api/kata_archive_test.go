package api

import (
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
	require.Error(t, err)
	resolutionAfterSwitch, err := service.Archive.Resolve(t.Context(), ref, "world")
	require.NoError(t, err)
	require.Equal(t, "failed", resolutionAfterSwitch.State)
	// A later context request verifies the source again.
	next, err := kataService(deps, gate)
	require.NoError(t, err)
	resolution, err := next.Archive.Resolve(t.Context(), ref, "world")
	require.NoError(t, err)
	require.Equal(t, "failed", resolution.State)
	// A new content hash for the first node also requires a new read.
	newHash, newSize, err := blobs.Write(strings.NewReader("hello again"))
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, newHash, newSize, "text/plain")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(blobsDir, newHash[:2], newHash)))
	selection.NodeID = node.ID
	_, err = service.Archive.Prepare(t.Context(), selection)
	require.Error(t, err)
}
