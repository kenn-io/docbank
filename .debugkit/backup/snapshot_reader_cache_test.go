package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestSnapshotReaderReusesAndClosesPacks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	repo := initTestRepo(t)
	known := map[pack.BlobID]IndexEntry{}
	var refs []ContentRef
	var entries []IndexEntry
	for i := range 66 {
		data := []byte(fmt.Sprintf("value-%d", i))
		appender := NewPackAppender(repo, known, pack.DefaultZstdLevel, nil, testPackExt)
		id, _, err := appender.Add(data)
		require.NoError(err)
		_, added, err := appender.Finish()
		require.NoError(err)
		entries = append(entries, added...)
		refs = append(refs, ContentRef{Hash: id.String(), Size: int64(len(data))})
	}
	firstID, err := pack.ParseBlobID(refs[0].Hash)
	require.NoError(err)
	whole := pack.ComputeBlobID([]byte("value-0value-0"))
	raw, err := json.Marshal(objectRecipe{
		Version: 1, Blob: whole.String(), Bytes: 14,
		Chunks: objectChunks{{Blob: firstID.String(), Bytes: 7}, {Blob: firstID.String(), Bytes: 7}},
	})
	require.NoError(err)
	appender := NewPackAppender(repo, known, pack.DefaultZstdLevel, nil, testPackExt)
	t.Cleanup(appender.Abort)
	recipeID, _, err := appender.Add(raw)
	require.NoError(err)
	refs = append(refs, ContentRef{Hash: whole.String(), Size: 14})
	list, err := EncodeAttachmentList(refs)
	require.NoError(err)
	listID, _, err := appender.Add(list)
	require.NoError(err)
	_, added, err := appender.Finish()
	require.NoError(err)
	_, err = repo.WriteIndex(append(entries, added...))
	require.NoError(err)
	m := testManifest("2026-01-01T00:00:00Z", "", 0)
	m.FormatVersion, m.MinReaderVersion = 5, 5
	m.Attachments = ManifestAttachments{Lists: []string{listID.String()}, Recipes: []string{recipeID.String()}}
	snapshot, err := repo.WriteManifest(m)
	require.NoError(err)
	reader, err := repo.OpenSnapshot(t.Context(), snapshot, testPackExt)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(reader.Close()) })
	_, err = reader.CopyObject(t.Context(), whole, io.Discard)
	require.NoError(err)
	first := reader.packs.readers[known[firstID].PackID]
	require.NotNil(first)
	for _, id := range []pack.BlobID{firstID, whole, firstID, whole} {
		_, err = reader.CopyObject(t.Context(), id, io.Discard)
		require.NoError(err)
		assert.Same(first, reader.packs.readers[known[firstID].PackID], "reuse the open descriptor and verified footer")
	}
	opened := map[*pack.Reader]pack.Entry{}
	for _, ref := range refs[:66] {
		id, parseErr := pack.ParseBlobID(ref.Hash)
		require.NoError(parseErr)
		_, err = reader.CopyObject(t.Context(), id, io.Discard)
		require.NoError(err)
		cached := reader.packs.readers[known[id].PackID]
		require.NotNil(cached)
		opened[cached.reader] = cached.entries[id]
		assert.LessOrEqual(len(reader.packs.readers), 64)
	}
	_, err = first.reader.ReadBlob(first.entries[firstID])
	require.ErrorIs(err, fs.ErrClosed, "eviction must close the descriptor")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = reader.CopyObject(ctx, whole, io.Discard)
	require.ErrorIs(err, context.Canceled)
	require.NoError(reader.Close())
	for handle, entry := range opened {
		_, readErr := handle.ReadBlob(entry)
		require.ErrorIs(readErr, fs.ErrClosed)
	}
	// New readers re-open descriptors and do not inherit closed cache entries.
	next, err := repo.OpenSnapshot(t.Context(), snapshot, testPackExt)
	require.NoError(err)
	var output bytes.Buffer
	_, err = next.CopyObject(t.Context(), whole, &output)
	require.NoError(err)
	assert.Equal("value-0value-0", output.String())
	require.NoError(next.Close())
}
