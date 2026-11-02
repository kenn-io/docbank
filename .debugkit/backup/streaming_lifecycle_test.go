package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/packstore/packstoretest"
)

type lifecycleContentSource struct{ store *packstore.Store }

func (s lifecycleContentSource) Open(ctx context.Context, ref ContentRef) (io.ReadCloser, error) {
	hash, err := packstore.ParseHash(ref.Hash)
	if err != nil {
		return nil, err
	}
	reader, _, err := s.store.OpenStream(ctx, hash)
	return reader, err
}

func addLifecycleCandidate(t *testing.T, catalog *packstoretest.MemoryCatalog, hash packstore.Hash, path string, size int64) {
	t.Helper()
	catalog.SetMember(hash, true)
	catalog.SetCandidate(packstore.Candidate{
		Hash: hash, OriginalHashes: []string{hash.String()}, Paths: []string{path}, Size: size,
	})
}

func readLifecycleBlob(t *testing.T, store *packstore.Store, hash packstore.Hash) []byte {
	t.Helper()
	reader, size, err := store.OpenStream(t.Context(), hash)
	require.NoError(t, err)
	content, readErr := io.ReadAll(reader)
	require.NoError(t, errors.Join(readErr, reader.Close()))
	assert.Equal(t, size, int64(len(content)))
	return content
}

// TestStreamingLifecycleGate exercises the integrated authority path that the
// focused package tests intentionally split apart: mixed reads, sparse repack,
// unpack, backup capture and verification, then loose and pack-native restore.
func TestStreamingLifecycleGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	dbPath, contentDir, dataDir, _ := seedBackupFixture(t)
	layout, err := packstore.NewLayout(contentDir, packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	require.NoError(err)
	catalog := packstoretest.NewMemoryCatalog()
	expected := make(map[packstore.Hash][]byte)

	err = filepath.WalkDir(contentDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		hash, parseErr := packstore.ParseHash(entry.Name())
		if parseErr != nil {
			return parseErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		addLifecycleCandidate(t, catalog, hash, path, info.Size())
		expected[hash] = content
		return nil
	})
	require.NoError(err)
	require.Len(expected, 2)

	loose, err := packstore.NewLooseStore(layout)
	require.NoError(err)
	for i := range 3 {
		content := bytes.Repeat([]byte{byte('x' + i)}, 4<<10)
		written, writeErr := loose.WriteBytes(ctx, content, packstore.WriteOptions{
			Durability: packstore.AtomicPublication, Dedup: packstore.VerifyFullHash,
		})
		require.NoError(writeErr)
		addLifecycleCandidate(t, catalog, written.Hash, written.Path, written.Size)
	}
	maintainer, err := packstore.NewMaintainer(catalog, layout, packstore.MaintainerOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(maintainer.Close()) })

	packed, err := maintainer.Pack(ctx, packstore.PackOptions{})
	require.NoError(err)
	assert.Equal(5, packed.BlobsPacked)
	for hash, want := range expected {
		assert.Equal(want, readLifecycleBlob(t, maintainer.Store(), hash))
	}

	mixed, err := loose.WriteBytes(ctx, []byte("mixed loose member"), packstore.WriteOptions{
		Durability: packstore.AtomicPublication, Dedup: packstore.VerifyFullHash,
	})
	require.NoError(err)
	addLifecycleCandidate(t, catalog, mixed.Hash, mixed.Path, mixed.Size)
	assert.Equal([]byte("mixed loose member"), readLifecycleBlob(t, maintainer.Store(), mixed.Hash))

	for hash := range catalog.Snapshot().Members {
		if _, keep := expected[hash]; !keep && hash != mixed.Hash {
			catalog.SetMember(hash, false)
		}
	}
	repacked, err := maintainer.Repack(ctx, packstore.RepackOptions{
		Now:       time.Now().Add(48 * time.Hour),
		Selection: packstore.RepackSelection{MinAge: time.Hour, MinDeadStored: 1},
	})
	require.NoError(err)
	assert.Equal(2, repacked.BlobsRepacked)

	unpacked, err := maintainer.Unpack(ctx)
	require.NoError(err)
	assert.Equal(2, unpacked.BlobsRestored)
	assert.Empty(catalog.Snapshot().Entries)
	for hash, want := range expected {
		assert.Equal(want, readLifecycleBlob(t, maintainer.Store(), hash))
	}

	repo := initTestRepo(t)
	app := packedExtensionApp{App: newTestApp()}
	manifest, err := Create(ctx, repo, app, CreateOptions{
		DBPath: dbPath, ContentDir: contentDir, DataDir: dataDir, CacheDir: t.TempDir(),
		Extras:        ExtrasSpec{Dirs: []ExtrasDirSpec{{Name: "deletions"}}},
		ContentSource: lifecycleContentSource{store: maintainer.Store()},
	})
	require.NoError(err)
	verified, err := Verify(ctx, repo, app, VerifyOptions{SnapshotID: manifest.SnapshotID})
	require.NoError(err)
	assert.Empty(verified.Problems)

	looseTarget := filepath.Join(t.TempDir(), "loose-restore")
	_, err = Restore(ctx, repo, app, RestoreOptions{TargetDir: looseTarget})
	require.NoError(err)
	for hash, want := range expected {
		path := filepath.Join(looseTarget, "content", hash.String()[:2], hash.String())
		got, readErr := os.ReadFile(path)
		require.NoError(readErr)
		assert.Equal(want, got)
	}

	packedTarget := filepath.Join(t.TempDir(), "packed-restore")
	restoredCatalog := packstoretest.NewMemoryCatalog()
	target := testPackedTarget{limits: packstore.DefaultLimits()}
	target.open = func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(_ context.Context, records []packstore.PackRecord, adoptions []packstore.Adoption) error {
			byPack := make(map[string][]packstore.IndexEntry)
			for _, adoption := range adoptions {
				restoredCatalog.SetMember(adoption.Entry.Hash, true)
				byPack[adoption.Entry.PackID] = append(byPack[adoption.Entry.PackID], adoption.Entry)
			}
			for _, record := range records {
				restoredCatalog.PutPack(record, byPack[record.PackID])
			}
			return nil
		}), nil
	}
	result, err := Restore(ctx, repo, app, RestoreOptions{TargetDir: packedTarget, PackedContent: target})
	require.NoError(err)
	assert.Equal(manifest.Attachments.Blobs, result.PackedAttachmentBlobs)
	packedLayout, err := packstore.NewLayout(filepath.Join(packedTarget, "content"), packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	require.NoError(err)
	restoredStore, err := packstore.NewStore(restoredCatalog, packedLayout, packstore.StoreOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(restoredStore.Close()) })
	for hash, want := range expected {
		assert.Equal(want, readLifecycleBlob(t, restoredStore, hash))
	}
	verified, err = Verify(ctx, repo, app, VerifyOptions{All: true})
	require.NoError(err)
	assert.Empty(verified.Problems)
}
