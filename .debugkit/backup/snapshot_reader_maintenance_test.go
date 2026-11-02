package backup_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
)

func TestSnapshotReaderYieldsToBackup(t *testing.T) {
	require := require.New(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	opts := backup.CreateOptions{MetadataSource: &portableSource{
		raw: []byte(`{"notes":[]}`), stats: []byte(`{"files":0}`), info: &backup.ContentInfo{},
	}}
	manifest, err := backup.Create(t.Context(), repo, portableApp{}, opts)
	require.NoError(err)
	reader, err := repo.OpenSnapshot(t.Context(), manifest.SnapshotID, ".kpack")
	require.NoError(err)
	entered, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var release sync.Once
	var workers sync.WaitGroup
	opts.Progress = func(event backup.ProgressEvent) {
		if event.Stage == backup.ProgressStageFreeze && !event.Final {
			close(entered)
			<-resume
		}
	}
	t.Cleanup(func() {
		assert.NoError(t, reader.Close())
		release.Do(func() { close(resume) })
		workers.Wait()
	})
	workers.Go(func() {
		_, createErr := backup.Create(t.Context(), repo, portableApp{}, opts)
		done <- createErr
	})
	// Observe the exclusive claim while the backup is waiting for this reader.
	require.Eventually(func() bool {
		_, statErr := os.Stat(repo.Path("locks", "exclusive.json"))
		return statErr == nil
	}, 30*time.Second, 10*time.Millisecond)
	blocked, err := repo.OpenSnapshot(t.Context(), manifest.SnapshotID, ".kpack")
	if blocked != nil {
		require.NoError(blocked.Close())
	}
	require.ErrorIs(err, backup.ErrRepoLocked)
	require.NoError(reader.Close())
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		require.FailNow("backup did not proceed after reader close")
	}
	blocked, err = repo.OpenSnapshot(t.Context(), manifest.SnapshotID, ".kpack")
	if blocked != nil {
		require.NoError(blocked.Close())
	}
	require.ErrorIs(err, backup.ErrRepoLocked)
	release.Do(func() { close(resume) })
	select {
	case createErr := <-done:
		require.NoError(createErr)
	case <-time.After(30 * time.Second):
		require.FailNow("backup did not finish")
	}
	next, err := repo.OpenSnapshot(t.Context(), manifest.SnapshotID, ".kpack")
	require.NoError(err)
	require.NoError(next.Close())
}

func TestSnapshotReaderReopensAfterPrune(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	inputs := t.TempDir()
	dead := make([]byte, 128<<10)
	_, err = rand.Read(dead)
	require.NoError(err)
	var refs []backup.ContentRef
	for _, data := range [][]byte{[]byte("retained content"), dead} {
		id := pack.ComputeBlobID(data)
		require.NoError(os.WriteFile(filepath.Join(inputs, id.String()), data, 0o600))
		refs = append(refs, backup.ContentRef{Hash: id.String(), StoragePath: id.String(), Size: int64(len(data))})
	}
	create := func(refs []backup.ContentRef) *backup.Manifest {
		stats, marshalErr := json.Marshal(portableStats{Files: int64(len(refs))})
		require.NoError(marshalErr)
		manifest, createErr := backup.Create(t.Context(), repo, portableApp{}, backup.CreateOptions{
			MetadataSource: &portableSource{raw: []byte(`{"notes":[]}`), stats: stats, info: &backup.ContentInfo{Refs: refs}},
			ContentDir:     inputs, Jobs: 1,
		})
		require.NoError(createErr)
		return manifest
	}
	older := create(refs)
	selected := create(refs[:1])
	id, err := pack.ParseBlobID(refs[0].Hash)
	require.NoError(err)
	before, err := repo.LoadBlobIndex()
	require.NoError(err)
	reader, err := repo.OpenSnapshot(t.Context(), selected.SnapshotID, ".kpack")
	require.NoError(err)
	_, err = reader.CopyObject(t.Context(), id, io.Discard)
	require.NoError(err)
	require.NoError(reader.Close())
	_, err = backup.Forget(t.Context(), repo, backup.ForgetOptions{SnapshotIDs: []string{older.SnapshotID}})
	require.NoError(err)
	pruned, err := backup.Prune(t.Context(), repo, portableApp{}, backup.PruneOptions{})
	require.NoError(err)
	require.NotEmpty(pruned.PacksToRepack)
	after, err := repo.LoadBlobIndex()
	require.NoError(err)
	assert.NotEqual(before[id].PackID, after[id].PackID, "fixture must relocate the selected object")
	reader, err = repo.OpenSnapshot(t.Context(), selected.SnapshotID, ".kpack")
	require.NoError(err)
	var output bytes.Buffer
	_, err = reader.CopyObject(t.Context(), id, &output)
	require.NoError(err)
	assert.Equal("retained content", output.String())
	require.NoError(reader.Close())
	// Forced maintenance is allowed to invalidate a reader's lease. Truncate
	// a needed pack while the descriptor is cached to exercise the failed read.
	reader, err = repo.OpenSnapshot(t.Context(), selected.SnapshotID, ".kpack")
	require.NoError(err)
	lock, err := repo.AcquireExclusiveLock("forced maintenance", true)
	require.NoError(err)
	entry := after[id]
	require.NoError(os.Truncate(repo.Path("packs", entry.PackID[:2], entry.PackID+".kpack"), int64(entry.Offset)))
	require.NoError(lock.Release())
	_, err = reader.CopyObject(t.Context(), id, io.Discard)
	require.Error(err)
	require.NoError(reader.Close())
}
