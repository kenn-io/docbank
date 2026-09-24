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

func TestEmbeddedProductionPackagePublishUsesOwnRootAndSurvivesReopen(t *testing.T) {
	storeVault, root, job := store.PublishedProductionJobHTTPFixture(t)
	require.NoError(t, storeVault.Close())
	abandoned := filepath.Join(root, "export-archives", ".production-package-abandoned")
	require.NoError(t, os.MkdirAll(abandoned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(abandoned, "synthetic.zip"),
		[]byte("abandoned synthetic bytes"), 0o600))
	first, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	_, err = os.Stat(abandoned)
	require.ErrorIs(t, err, os.ErrNotExist)
	second, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	request := docbank.ProductionPackagePublishRequest{
		OperationID: "77777777-7777-4777-8777-777777777777",
		ProfileID:   "export-dat-opt-images-v1", MaxVolumeBytes: 50 << 20,
		MaxVolumeDocuments: 10,
	}
	_, err = second.PublishProductionPackage(t.Context(), job.ID, request)
	require.ErrorIs(t, err, store.ErrNotFound)
	published, err := first.PublishProductionPackage(t.Context(), job.ID, request)
	require.NoError(t, err)
	require.Equal(t, job.ID, published.JobID)
	require.Equal(t, request.OperationID, published.OperationID)
	require.Equal(t, request.ProfileID, published.ProfileID)
	require.NotEmpty(t, published.EvidenceSHA256)
	replayed, err := first.PublishProductionPackage(t.Context(), job.ID, request)
	require.NoError(t, err)
	require.Equal(t, published, replayed)
	requireNoEmbeddedPublishStages(t, root)
	var archive bytes.Buffer
	receipt, err := first.DownloadProductionPackageTo(t.Context(), job.ID, request.OperationID, &archive)
	require.NoError(t, err)
	require.Equal(t, published.VersionID, receipt.VersionID)
	require.Equal(t, published.ArchiveSHA256, receipt.ArchiveSHA256)
	require.Equal(t, published.EvidenceSHA256, receipt.EvidenceSHA256)
	require.Equal(t, published.Size, int64(archive.Len()))
	sum := sha256.Sum256(archive.Bytes())
	require.Equal(t, published.ArchiveSHA256, hex.EncodeToString(sum[:]))
	changed := request
	changed.ProfileID = "export-dat-pdf-v1"
	_, err = first.PublishProductionPackage(t.Context(), job.ID, changed)
	require.Error(t, err)
	requireNoEmbeddedPublishStages(t, root)
	require.NoError(t, first.Close())
	first, err = docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	replayed, err = first.PublishProductionPackage(t.Context(), job.ID, request)
	require.NoError(t, err)
	require.Equal(t, published, replayed)
	requireNoEmbeddedPublishStages(t, root)
}

func requireNoEmbeddedPublishStages(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "export-archives"))
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".production-package-"),
			"embedded package staging must be removed")
	}
}

func TestEmbeddedProductionPackageSweepRejectsLinkedArchiveDirectory(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	abandoned := filepath.Join(external, ".production-package-external")
	require.NoError(t, os.Mkdir(abandoned, 0o700))
	marker := filepath.Join(abandoned, "synthetic.zip")
	require.NoError(t, os.WriteFile(marker, []byte("synthetic external bytes"), 0o600))
	if err := os.Symlink(external, filepath.Join(root, "export-archives")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	if err == nil {
		t.Cleanup(func() { require.NoError(t, vault.Close()) })
	}
	require.Error(t, err)
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, []byte("synthetic external bytes"), data)
}
