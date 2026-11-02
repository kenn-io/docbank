package backup_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
)

func TestSnapshotReaderCopiesSelectedObjects(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	inputs := t.TempDir()
	var refs []backup.ContentRef
	var extras []backup.ExtrasFileSpec
	var ids []pack.BlobID
	for i, size := range []int{0, 37, 64 << 20, 64<<20 + 1} {
		data := bytes.Repeat([]byte{byte(i + 1)}, size)
		id := pack.BlobID(sha256.Sum256(data))
		path := filepath.Join(inputs, id.String())
		require.NoError(os.WriteFile(path, data, 0o600))
		ids = append(ids, id)
		refs = append(refs, backup.ContentRef{Hash: id.String(), Size: int64(size), StoragePath: id.String()})
		extras = append(extras, backup.ExtrasFileSpec{Path: path, RecordAs: id.String()})
	}
	manifest, err := backup.Create(t.Context(), repo, portableApp{}, backup.CreateOptions{
		MetadataSource: &portableSource{raw: []byte(`{"notes":[]}`), stats: []byte(`{"files":4}`), info: &backup.ContentInfo{Refs: refs}},
		ContentDir:     inputs, Extras: backup.ExtrasSpec{Files: extras}, Jobs: 1,
	})
	require.NoError(err)
	require.Len(manifest.Attachments.Recipes, 1)
	require.Len(manifest.Extras.Recipes, 1)
	require.NoError(os.RemoveAll(inputs))
	reader, err := repo.OpenSnapshot(t.Context(), manifest.SnapshotID, ".kpack")
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(reader.Close()) })
	assert.Equal(repo.Config().RepoID, reader.Info().RepositoryID)
	assert.Equal(manifest.SnapshotID, reader.Info().SnapshotID)
	assert.Equal(6, reader.Info().MinReaderVersion)
	for i, id := range ids {
		object, lookupErr := reader.Object(id)
		require.NoError(lookupErr)
		assert.Equal(refs[i].Size, object.Bytes)
		extra, lookupErr := reader.Extra(id.String())
		require.NoError(lookupErr)
		assert.Equal(object, extra)
		for _, copyObject := range []func(context.Context, io.Writer) (int64, error){
			func(ctx context.Context, dst io.Writer) (int64, error) { return reader.CopyObject(ctx, id, dst) },
			func(ctx context.Context, dst io.Writer) (int64, error) {
				return reader.CopyExtra(ctx, id.String(), dst)
			},
		} {
			output, openErr := os.CreateTemp(t.TempDir(), "copied")
			require.NoError(openErr)
			n, copyErr := copyObject(t.Context(), output)
			require.NoError(copyErr)
			assert.Equal(refs[i].Size, n)
			_, seekErr := output.Seek(0, io.SeekStart)
			require.NoError(seekErr)
			digest := sha256.New()
			n, readErr := io.Copy(digest, output)
			require.NoError(readErr)
			assert.Equal(refs[i].Size, n)
			assert.Equal(id[:], digest.Sum(nil))
			require.NoError(output.Close())
		}
	}
	metadataID, err := pack.ParseBlobID(manifest.Metadata.Blob)
	require.NoError(err)
	var output bytes.Buffer
	_, err = reader.CopyObject(t.Context(), metadataID, &output)
	require.ErrorIs(err, fs.ErrNotExist)
	assert.Empty(output.Bytes())
	_, err = reader.CopyExtra(t.Context(), "missing", &output)
	require.ErrorIs(err, fs.ErrNotExist)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = reader.CopyObject(ctx, ids[3], io.Discard)
	require.ErrorIs(err, context.Canceled)
	for _, id := range []pack.BlobID{ids[1], ids[3]} {
		ctx, cancel = context.WithCancel(t.Context())
		_, err = reader.CopyObject(ctx, id, snapshotCancelWriter{cancel})
		cancel()
		require.ErrorIs(err, context.Canceled, "cancellation during output must fail the copy")
	}
	writeErr := errors.New("destination full")
	_, err = reader.CopyObject(t.Context(), ids[3], snapshotFailWriter{writeErr})
	require.ErrorIs(err, writeErr)
	require.NoError(reader.Close())
	_, err = reader.Object(ids[3])
	require.NoError(err, "membership metadata survives close")
	_, err = reader.CopyObject(t.Context(), ids[3], io.Discard)
	require.ErrorIs(err, fs.ErrClosed)
}

type snapshotFailWriter struct{ err error }

func (w snapshotFailWriter) Write([]byte) (int, error) { return 0, w.err }

type snapshotCancelWriter struct{ cancel context.CancelFunc }

func (w snapshotCancelWriter) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}
