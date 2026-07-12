package blob_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

type deferredRetirementCatalog struct {
	packstore.Catalog

	blobsDir string
	heldPath string
	packPath string
}

func (c *deferredRetirementCatalog) CommitRepack(
	ctx context.Context, sourceIDs []string, records []packstore.PackRecord, moves []packstore.RepackMove,
) error {
	if err := c.Catalog.CommitRepack(ctx, sourceIDs, records, moves); err != nil {
		return fmt.Errorf("committing test repack: %w", err)
	}
	if len(sourceIDs) != 1 {
		return fmt.Errorf("test retirement hook expected one source pack, got %d", len(sourceIDs))
	}
	c.packPath = filepath.Join(c.blobsDir, "packs", sourceIDs[0][:2], sourceIDs[0]+packstore.PackExt)
	c.heldPath = c.packPath + ".held"
	if err := os.Rename(c.packPath, c.heldPath); err != nil {
		return fmt.Errorf("holding retired source pack: %w", err)
	}
	if err := os.Mkdir(c.packPath, 0o700); err != nil {
		return fmt.Errorf("blocking retired source path: %w", err)
	}
	return os.WriteFile(filepath.Join(c.packPath, "lock"), []byte("held"), 0o600)
}

func (c *deferredRetirementCatalog) release() error {
	if c.heldPath == "" {
		return nil
	}
	if _, err := os.Stat(c.heldPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.RemoveAll(c.packPath); err != nil {
		return err
	}
	return os.Rename(c.heldPath, c.packPath)
}

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

func TestMaintainerSharesDocbankMutationCoordinator(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	physical, err := blob.New(store.NewPackCatalog(metadata), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, physical.Close()) })

	require.NoError(t, physical.WithMutation(ctx, func() error {
		hash, size, writeErr := physical.WriteContext(ctx, strings.NewReader("coordinated content"))
		if writeErr != nil {
			return writeErr
		}
		_, createErr := metadata.CreateFile(ctx, metadata.RootID(), "coordinated.txt",
			hash, size, "text/plain")
		return createErr
	}))

	lease, err := physical.Coordinator().AcquireMutation(ctx)
	require.NoError(t, err)
	waitCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	_, err = physical.Maintainer().Pack(waitCtx, packstore.PackOptions{})
	cancel()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, lease.Release())

	packed, err := physical.Maintainer().Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, packed.BlobsPacked)
}

func TestActiveStreamSurvivesRepackRetirementAndStoreClose(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	physical, err := blob.New(store.NewPackCatalog(metadata), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, physical.Close()) })

	liveContent := bytes.Repeat([]byte("leased packed content "), 8192)
	var liveHash string
	var dead []store.Node
	require.NoError(t, physical.WithMutation(ctx, func() error {
		var writeErr error
		liveHash, _, writeErr = physical.WriteContext(ctx, bytes.NewReader(liveContent))
		if writeErr != nil {
			return writeErr
		}
		_, createErr := metadata.CreateFile(ctx, metadata.RootID(), "live.bin",
			liveHash, int64(len(liveContent)), "application/octet-stream")
		if createErr != nil {
			return createErr
		}
		for i, content := range []string{"first dead content", "second dead content"} {
			deadHash, deadSize, err := physical.WriteContext(ctx, strings.NewReader(content))
			if err != nil {
				return err
			}
			node, err := metadata.CreateFile(ctx, metadata.RootID(),
				fmt.Sprintf("dead-%d.txt", i), deadHash, deadSize, "text/plain")
			if err != nil {
				return err
			}
			dead = append(dead, node)
		}
		return nil
	}))

	packed, err := physical.Maintainer().Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.PacksSealed)
	records, err := store.NewPackCatalog(metadata).ListPackRecords(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1)
	oldPackPath := filepath.Join(blobsDir, "packs", records[0].PackID[:2],
		records[0].PackID+packstore.PackExt)

	stream, size, err := physical.OpenStreamContext(ctx, liveHash)
	require.NoError(t, err)
	assert.Equal(t, int64(len(liveContent)), size)
	prefix := make([]byte, 97)
	_, err = io.ReadFull(stream, prefix)
	require.NoError(t, err)
	assert.False(t, stream.Verified())

	require.NoError(t, physical.WithMutation(ctx, func() error {
		for _, node := range dead {
			if _, _, trashErr := metadata.Trash(ctx, node.ID, -1); trashErr != nil {
				return trashErr
			}
		}
		if _, emptyErr := metadata.TrashEmpty(ctx, 0, true); emptyErr != nil {
			return emptyErr
		}
		return metadata.DeleteBlobRows(ctx, []string{dead[0].BlobHash, dead[1].BlobHash})
	}))
	repacked, err := physical.Maintainer().Repack(ctx, packstore.RepackOptions{
		Now: time.Now().UTC().Add(48 * time.Hour),
		Selection: packstore.RepackSelection{
			MinAge: time.Nanosecond, MinDeadStored: 1,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, repacked.PacksRewritten)
	assert.Equal(t, 1, repacked.PacksRemoved)
	assert.NoFileExists(t, oldPackPath)
	require.NoError(t, physical.Close())

	rest, err := io.ReadAll(stream)
	require.NoError(t, err)
	got := make([]byte, 0, len(prefix)+len(rest))
	got = append(got, prefix...)
	got = append(got, rest...)
	assert.Equal(t, liveContent, got)
	assert.True(t, stream.Verified())
	require.NoError(t, stream.Close())
}

func TestDeferredRetirementCommitsAuthorityAndPackReconcilesOrphan(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	baseCatalog := store.NewPackCatalog(metadata)
	catalog := &deferredRetirementCatalog{Catalog: baseCatalog, blobsDir: blobsDir}
	physical, err := blob.New(catalog, blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, physical.Close()) })
	t.Cleanup(func() { require.NoError(t, catalog.release()) })

	var nodes []store.Node
	require.NoError(t, physical.WithMutation(ctx, func() error {
		for i, content := range []string{"live after deferred retirement", "dead one", "dead two"} {
			hash, size, writeErr := physical.WriteContext(ctx, strings.NewReader(content))
			if writeErr != nil {
				return writeErr
			}
			node, createErr := metadata.CreateFile(ctx, metadata.RootID(), fmt.Sprintf("item-%d.txt", i),
				hash, size, "text/plain")
			if createErr != nil {
				return createErr
			}
			nodes = append(nodes, node)
		}
		return nil
	}))
	packed, err := physical.Maintainer().Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.PacksSealed)
	records, err := baseCatalog.ListPackRecords(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1)
	sourceID := records[0].PackID

	require.NoError(t, physical.WithMutation(ctx, func() error {
		for _, node := range nodes[1:] {
			if _, _, trashErr := metadata.Trash(ctx, node.ID, -1); trashErr != nil {
				return trashErr
			}
		}
		if _, emptyErr := metadata.TrashEmpty(ctx, 0, true); emptyErr != nil {
			return emptyErr
		}
		return metadata.DeleteBlobRows(ctx, []string{nodes[1].BlobHash, nodes[2].BlobHash})
	}))
	repacked, err := physical.Maintainer().Repack(ctx, packstore.RepackOptions{
		Now: time.Now().UTC().Add(48 * time.Hour),
		Selection: packstore.RepackSelection{
			MinAge: time.Nanosecond, MinDeadStored: 1,
		},
	})
	require.ErrorIs(t, err, packstore.ErrPackRetirementDeferred)
	assert.Equal(t, 1, repacked.PacksRewritten)
	assert.Zero(t, repacked.PacksRemoved)
	hasSource, err := baseCatalog.HasPackRecord(ctx, sourceID)
	require.NoError(t, err)
	assert.False(t, hasSource, "the committed replacement must not be rolled back")
	assertBlobContent(t, physical, nodes[0].BlobHash, "live after deferred retirement")

	require.NoError(t, catalog.release())
	reconciled, err := physical.Maintainer().Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, reconciled.PacksRemoved)
	assert.NoFileExists(t, catalog.packPath)
	assertBlobContent(t, physical, nodes[0].BlobHash, "live after deferred retirement")
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
