package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

type stagedPackageCatalog struct {
	packageRecord store.RetainedProductionPackage
}

func (c stagedPackageCatalog) LoadRetainedProductionPackage(context.Context, string, string) (store.RetainedProductionPackage, error) {
	return c.packageRecord, nil
}

type stagedPackageStream struct{ *bytes.Reader }

func (s *stagedPackageStream) Close() error { return nil }
func (s *stagedPackageStream) Verify() error {
	if s.Len() != 0 {
		return errors.New("synthetic stream not fully consumed")
	}
	return nil
}
func (s *stagedPackageStream) Verified() bool { return s.Len() == 0 }

type stagedPackageBlobs struct{ data map[string][]byte }

func (b stagedPackageBlobs) OpenStreamContext(_ context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	data, ok := b.data[hash]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return &stagedPackageStream{Reader: bytes.NewReader(data)}, int64(len(data)), nil
}

func TestStageRetainedProductionPackageBlobsVerifiesAndCleansPrivateBytes(t *testing.T) {
	parts := [3][]byte{[]byte("synthetic ZIP"), []byte("synthetic QC"), []byte("synthetic transmittal")}
	record := store.RetainedProductionPackage{}
	bound := []*store.ContentWriteReceipt{&record.Archive, &record.QC, &record.Transmittal}
	blobs := stagedPackageBlobs{data: make(map[string][]byte)}
	for index, data := range parts {
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		bound[index].Version.BlobHash = hash
		bound[index].Version.Size = int64(len(data))
		blobs.data[hash] = data
	}
	root := t.TempDir()
	staged, err := StageRetainedProductionPackageBlobs(t.Context(),
		stagedPackageCatalog{record}, blobs, root,
		"77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888")
	require.NoError(t, err)
	for index, path := range []string{staged.ArchivePath, staged.QCPath, staged.TransmittalPath} {
		actual, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.Equal(t, parts[index], actual)
		require.Equal(t, filepath.Dir(staged.ArchivePath), filepath.Dir(path))
	}
	require.NoError(t, staged.Close())
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)

	bad := stagedPackageBlobs{data: maps.Clone(blobs.data)}
	bad.data[record.QC.Version.BlobHash] = []byte("wrong QC bytes")
	_, err = StageRetainedProductionPackageBlobs(t.Context(), stagedPackageCatalog{record}, bad,
		root, "77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888")
	require.Error(t, err)
	entries, err = os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
	delete(blobs.data, record.Transmittal.Version.BlobHash)
	_, err = StageRetainedProductionPackageBlobs(t.Context(), stagedPackageCatalog{record}, blobs,
		root, "77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888")
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err = os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = StageRetainedProductionPackageBlobs(canceled, stagedPackageCatalog{record}, bad,
		root, "77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888")
	require.ErrorIs(t, err, context.Canceled)
	entries, err = os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}
