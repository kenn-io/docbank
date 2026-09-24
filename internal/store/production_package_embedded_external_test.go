package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddedProductionPackageDownloadUsesOwnRootAndVerifiesBytes(t *testing.T) {
	vault, root, job, retained := store.PublishedProductionPackageHTTPFixture(t)
	require.NoError(t, vault.Close())
	first, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	var wrongRoot bytes.Buffer
	_, err = second.DownloadProductionPackageTo(t.Context(), job.ID, retained.Evidence.ID, &wrongRoot)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.Zero(t, wrongRoot.Len())
	var archive bytes.Buffer
	receipt, err := first.DownloadProductionPackageTo(t.Context(), job.ID, retained.Evidence.ID, &archive)
	require.NoError(t, err)
	require.Equal(t, retained.Archive.Version.ID, receipt.VersionID)
	require.Equal(t, retained.Archive.Version.BlobHash, receipt.ArchiveSHA256)
	require.Equal(t, retained.Archive.Version.Size, receipt.Size)
	require.Equal(t, retained.Evidence.SHA256, receipt.EvidenceSHA256)
	require.Equal(t, receipt.Size, int64(archive.Len()))
	sum := sha256.Sum256(archive.Bytes())
	require.Equal(t, receipt.ArchiveSHA256, hex.EncodeToString(sum[:]))
	requireNoEmbeddedPackageStages(t, root)
	require.NoError(t, os.Remove(filepath.Join(root, "blobs", receipt.ArchiveSHA256[:2], receipt.ArchiveSHA256)))
	var missing bytes.Buffer
	_, err = first.DownloadProductionPackageTo(t.Context(), job.ID, retained.Evidence.ID, &missing)
	require.Error(t, err)
	require.Zero(t, missing.Len(), "unverified bytes must not reach the destination")
	requireNoEmbeddedPackageStages(t, root)
}

func TestEmbeddedProductionPackageSweepsAbandonedStageOnOpen(t *testing.T) {
	root := t.TempDir()
	abandoned := filepath.Join(root, ".production-download-abandoned")
	require.NoError(t, os.Mkdir(abandoned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(abandoned, "synthetic-package.zip"),
		[]byte("abandoned synthetic bytes"), 0o600))
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	_, err = os.Stat(abandoned)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func requireNoEmbeddedPackageStages(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".production-download-"),
			"embedded package staging must be removed")
	}
}
