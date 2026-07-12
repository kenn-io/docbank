package blob_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func TestMixedStoreLifecyclePreservesMembershipAndBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	physical, err := blob.New(store.NewPackCatalog(metadata), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, physical.Close()) })

	type fixture struct {
		node store.Node
		hash string
		data string
	}
	fixtures := make([]fixture, 0, 3)
	err = physical.WithMutation(ctx, func() error {
		for i, content := range []string{
			"alpha packed document",
			"bravo packed document",
			"charlie packed document",
		} {
			hash, size, writeErr := physical.WriteContext(ctx, strings.NewReader(content))
			if writeErr != nil {
				return writeErr
			}
			node, createErr := metadata.CreateFile(ctx, metadata.RootID(), string(rune('a'+i))+".txt",
				hash, size, "text/plain")
			if createErr != nil {
				return createErr
			}
			fixtures = append(fixtures, fixture{node: node, hash: hash, data: content})
		}
		return nil
	})
	require.NoError(t, err)

	// Existing loose content is immediately readable and startup performs no
	// eager conversion. Packing happens only when maintenance is requested.
	for _, item := range fixtures {
		assert.FileExists(t, filepath.Join(blobsDir, item.hash[:2], item.hash))
		assertBlobContent(t, physical, item.hash, item.data)
	}
	catalog := store.NewPackCatalog(metadata)
	records, err := catalog.ListPackRecords(ctx)
	require.NoError(t, err)
	assert.Empty(t, records)

	packed, err := physical.Maintainer().Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, packed.BlobsPacked)
	assert.Equal(t, 1, packed.PacksSealed)
	for _, item := range fixtures {
		assert.NoFileExists(t, filepath.Join(blobsDir, item.hash[:2], item.hash))
		assertBlobContent(t, physical, item.hash, item.data)
	}

	// Removing two nodes and their blob rows revokes read authority and their
	// mappings. The immutable source pack remains truthful until repack swaps
	// the survivor and retires it.
	err = physical.WithMutation(ctx, func() error {
		for _, item := range fixtures[:2] {
			if _, _, trashErr := metadata.Trash(ctx, item.node.ID, -1); trashErr != nil {
				return trashErr
			}
		}
		if _, emptyErr := metadata.TrashEmpty(ctx, 0, true); emptyErr != nil {
			return emptyErr
		}
		return metadata.DeleteBlobRows(ctx, []string{fixtures[0].hash, fixtures[1].hash})
	})
	require.NoError(t, err)
	for _, item := range fixtures[:2] {
		_, err := physical.Open(item.hash)
		require.ErrorIs(t, err, os.ErrNotExist)
	}

	repacked, err := physical.Maintainer().Repack(ctx, packstore.RepackOptions{
		Now: time.Now().UTC().Add(48 * time.Hour),
		Selection: packstore.RepackSelection{
			MinAge: time.Nanosecond, MinDeadStored: 1,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, repacked.PacksRewritten)
	assert.Equal(t, 1, repacked.PacksRemoved)
	assertBlobContent(t, physical, fixtures[2].hash, fixtures[2].data)

	unpacked, err := physical.Maintainer().Unpack(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, unpacked.BlobsRestored)
	assert.FileExists(t, filepath.Join(blobsDir, fixtures[2].hash[:2], fixtures[2].hash))
	entries, err := catalog.ListIndexed(ctx)
	require.NoError(t, err)
	assert.Empty(t, entries)
	records, err = catalog.ListPackRecords(ctx)
	require.NoError(t, err)
	assert.Empty(t, records)
	assertBlobContent(t, physical, fixtures[2].hash, fixtures[2].data)
}

func assertBlobContent(t *testing.T, physical *blob.Store, hash, want string) {
	t.Helper()
	reader, size, err := physical.OpenStream(hash)
	require.NoError(t, err)
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	assert.True(t, reader.Verified())
	assert.Equal(t, int64(len(data)), size)
	assert.Equal(t, want, string(data))
}
