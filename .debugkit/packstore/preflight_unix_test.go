//go:build unix

package packstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestPreflightRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pack-fifo")
	require.NoError(t, unix.Mkfifo(path, 0o600))
	assertFIFOOperationDoesNotBlock(t, path, func() error {
		reader, err := OpenMaintenancePack(path, DefaultLimits())
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		return err
	})
}

func TestLooseReadsRejectFIFOWithoutBlocking(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		name := "open"
		if bounded {
			name = "bounded"
		}
		t.Run(name, func(t *testing.T) {
			layout := layoutForStoreTest(t)
			hash := hashForTest([]byte("fifo content"))
			path := layout.LoosePath(hash)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, unix.Mkfifo(path, 0o600))
			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
				hash: {Member: true},
			}}, layout)

			assertFIFOOperationDoesNotBlock(t, path, func() error {
				if bounded {
					_, _, err := store.ReadBounded(t.Context(), hash, DefaultLimits().BlobBytes)
					return err
				}
				reader, _, err := store.Open(t.Context(), hash)
				if reader != nil {
					err = errors.Join(err, reader.Close())
				}
				return err
			})
		})
	}
}

func TestOrdinaryPackedReadRejectsFIFOWithoutBlocking(t *testing.T) {
	layout := layoutForStoreTest(t)
	entry := buildStoreTestPack(t, layout, []byte("ordinary packed fifo content"))
	path := layout.PackPath(entry.PackID)
	require.NoError(t, os.Remove(path))
	require.NoError(t, unix.Mkfifo(path, 0o600))
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout)

	assertFIFOOperationDoesNotBlock(t, path, func() error {
		reader, _, err := store.Open(t.Context(), entry.Hash)
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		return err
	})
}

func TestPreflightRejectsFIFOReplacedAfterIdentitySnapshot(t *testing.T) {
	layout := layoutForStoreTest(t)
	entry := buildStoreTestPack(t, layout, []byte("replace pack with fifo"))
	path := layout.PackPath(entry.PackID)
	originalSnapshot := snapshotBoundedPackPathIdentity
	snapshotBoundedPackPathIdentity = func(path string) (fs.FileInfo, error) {
		info, err := snapshotPathIdentity(path)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if err := unix.Mkfifo(path, 0o600); err != nil {
			return nil, err
		}
		return info, nil
	}
	t.Cleanup(func() { snapshotBoundedPackPathIdentity = originalSnapshot })

	assertFIFOOperationDoesNotBlock(t, path, func() error {
		reader, err := OpenMaintenancePack(path, DefaultLimits())
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		return err
	})
}

func assertFIFOOperationDoesNotBlock(t *testing.T, path string, operation func() error) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- operation() }()

	select {
	case err := <-done:
		require.Error(err)
	case <-time.After(time.Second):
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		require.NoError(err)
		require.NoError(writer.Close())
		<-done
		assert.Fail(t, "filesystem read blocked opening a FIFO")
	}
}
