package exporter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func TestStartupRemovesOnlyAbandonedProductionPackageStaging(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".production-package-abandoned")
	require.NoError(t, os.Mkdir(stale, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "recipient.zip"), []byte("synthetic package"), 0o600))
	keep := filepath.Join(root, "other-private-work")
	require.NoError(t, os.Mkdir(keep, 0o700))
	w := &Worker{dir: root}
	require.NoError(t, w.cleanupAbandonedArchives(t.Context()))
	_, err := os.Stat(stale)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(keep)
	require.NoError(t, err)
}

func TestProductionPackageWriterRecordsExactDurableBytes(t *testing.T) {
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	w := &Worker{catalog: catalog, blobs: blobs}
	data := []byte("synthetic recipient archive bytes")
	var hash string
	var size int64
	err = blobs.WithMutation(t.Context(), func() error {
		var physical store.BlobPhysical
		var writeErr error
		hash, size, physical, writeErr = w.writeProductionPackageBlob(t.Context(), bytes.NewReader(data))
		if writeErr != nil {
			return writeErr
		}
		return catalog.RecordBlob(t.Context(), hash, size, physical)
	})
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	require.Equal(t, hex.EncodeToString(sum[:]), hash)
	require.Equal(t, int64(len(data)), size)
	stream, actualSize, err := blobs.OpenStreamContext(t.Context(), hash)
	require.NoError(t, err)
	actual, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())
	require.Equal(t, size, actualSize)
	require.Equal(t, data, actual)
}
