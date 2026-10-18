package docbank_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank/docbank"
)

func TestBackupReaderWithoutVault(t *testing.T) {
	size := int64(64<<20 + 1)
	if value := os.Getenv("DOCBANK_LARGE_OBJECT_TEST_BYTES"); value != "" {
		var err error
		size, err = strconv.ParseInt(value, 10, 64)
		require.NoError(t, err)
		require.Greater(t, size, int64(64<<20))
	}
	source := filepath.Join(t.TempDir(), "source")
	vault, err := docbank.New(t.Context(), docbank.Config{Root: source})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, vault.Close()) })
	inputs := t.TempDir()
	var hashes []string
	var extraPath string
	for i, name := range []string{"content.bin", "catalog.bin"} {
		file, createErr := os.Create(filepath.Join(inputs, name))
		require.NoError(t, createErr)
		t.Cleanup(func() { _ = file.Close() })
		require.NoError(t, file.Truncate(size))
		_, err = file.WriteAt([]byte(name), 0)
		require.NoError(t, err)
		_, err = file.WriteAt([]byte("tail"), size-4)
		require.NoError(t, err)
		digest := sha256.New()
		n, hashErr := io.Copy(digest, file)
		require.NoError(t, hashErr)
		require.Equal(t, size, n)
		hash := hex.EncodeToString(digest.Sum(nil))
		hashes = append(hashes, hash)
		if i == 0 {
			_, err = file.Seek(0, io.SeekStart)
			require.NoError(t, err)
			_, err = vault.Create(t.Context(), "/content.bin", file, docbank.CreateOptions{
				MediaType: "application/octet-stream", Expected: docbank.ContentIdentity{SHA256: hash, Size: size},
			})
			require.NoError(t, err)
		} else {
			extraPath = file.Name()
		}
		require.NoError(t, file.Close())
	}
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	snapshot, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
		ExtraFiles: []docbank.BackupExtraFile{{Path: extraPath, RecordAs: "application/catalog.bin"}},
	})
	require.NoError(t, err)
	later := createRangeFixture(t, vault, "/later.txt", []byte("only in the later snapshot"))
	newer, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	require.NoError(t, os.RemoveAll(source))
	require.NoError(t, os.RemoveAll(inputs))
	repository, err = docbank.OpenBackupRepository(repository.Root())
	require.NoError(t, err)
	reader, err := repository.OpenSnapshot(t.Context(), snapshot.ID)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reader.Close()) })
	assert.Equal(t, repository.ID(), reader.Info().RepositoryID)
	assert.Equal(t, snapshot.ID, reader.Info().SnapshotID)
	assert.Equal(t, snapshot.MinReaderVersion, reader.Info().MinReaderVersion)
	object, err := reader.Object(hashes[0])
	require.NoError(t, err)
	assert.Equal(t, docbank.BackupObject{SHA256: hashes[0], Bytes: size}, object)
	extra, err := reader.Extra("application/catalog.bin")
	require.NoError(t, err)
	assert.Equal(t, docbank.BackupObject{SHA256: hashes[1], Bytes: size}, extra)
	for i, copyFile := range []func(io.Writer) (int64, error){
		func(dst io.Writer) (int64, error) { return reader.CopyObject(t.Context(), hashes[0], dst) },
		func(dst io.Writer) (int64, error) {
			return reader.CopyExtra(t.Context(), "application/catalog.bin", dst)
		},
	} {
		digest := sha256.New()
		n, copyErr := copyFile(digest)
		require.NoError(t, copyErr)
		assert.Equal(t, size, n)
		assert.Equal(t, hashes[i], hex.EncodeToString(digest.Sum(nil)))
	}
	var output bytes.Buffer
	_, err = reader.CopyObject(t.Context(), later.Computed.SHA256, &output)
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Empty(t, output.Bytes())
	_, err = reader.Extra("missing")
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = reader.CopyExtra(t.Context(), "missing", &output)
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Empty(t, output.Bytes())
	for _, hash := range []string{"", "not-a-hash", "../content"} {
		_, err = reader.Object(hash)
		require.Error(t, err)
		_, err = reader.CopyObject(t.Context(), hash, &output)
		require.Error(t, err)
		assert.Empty(t, output.Bytes())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = reader.CopyObject(ctx, hashes[0], io.Discard)
	require.ErrorIs(t, err, context.Canceled)
	writeErr := errors.New("destination full")
	_, err = reader.CopyExtra(t.Context(), "application/catalog.bin", backupReaderWriter(func([]byte) (int, error) { return 0, writeErr }))
	require.ErrorIs(t, err, writeErr)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	copied, err := reader.CopyObject(ctx, hashes[0], backupReaderWriter(func(p []byte) (int, error) { cancel(); return len(p), nil }))
	require.ErrorIs(t, err, context.Canceled)
	assert.Positive(t, copied, "a failed copy still reports bytes already written")
	require.NoError(t, reader.Close())
	_, err = repository.Forget(t.Context(), docbank.BackupForgetOptions{SnapshotIDs: []string{newer.ID}})
	require.NoError(t, err)
	_, err = repository.Prune(t.Context(), docbank.BackupPruneOptions{})
	require.NoError(t, err)
	reopened, err := repository.OpenSnapshot(t.Context(), snapshot.ID)
	require.NoError(t, err)
	digest := sha256.New()
	_, err = reopened.CopyObject(t.Context(), hashes[0], digest)
	require.NoError(t, err)
	assert.Equal(t, hashes[0], hex.EncodeToString(digest.Sum(nil)))
	require.NoError(t, reopened.Close())
	assert.NoDirExists(t, source)
	assert.NoFileExists(t, filepath.Join(repository.Root(), "docbank.db"))
	assert.NoDirExists(t, filepath.Join(repository.Root(), "blobs"))
	require.NoError(t, os.RemoveAll(repository.Root()))
	afterClose, err := reader.Object(hashes[0])
	require.NoError(t, err)
	assert.Equal(t, object, afterClose)
	afterClose, err = reader.Extra("application/catalog.bin")
	require.NoError(t, err)
	assert.Equal(t, extra, afterClose)
	assert.Equal(t, snapshot.ID, reader.Info().SnapshotID)
	_, err = reader.CopyObject(t.Context(), hashes[0], io.Discard)
	require.ErrorIs(t, err, fs.ErrClosed)
	require.NoError(t, reader.Close())
}

type backupReaderWriter func([]byte) (int, error)

func (w backupReaderWriter) Write(p []byte) (int, error) { return w(p) }

func TestBackupReaderBackupContention(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(t.TempDir(), "source")})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, vault.Close()) })
	createRangeFixture(t, vault, "/content.txt", []byte("archived content"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	snapshot, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{})
	require.NoError(t, err)
	paused, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var workers sync.WaitGroup
	done := make(chan error, 1)
	t.Cleanup(func() { once.Do(func() { close(resume) }); workers.Wait() })
	workers.Go(func() {
		_, createErr := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
			Prepare: func(context.Context) error { close(paused); <-resume; return nil },
		})
		done <- createErr
	})
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "backup did not reach host preparation")
	}
	_, err = repository.OpenSnapshot(t.Context(), snapshot.ID)
	require.ErrorIs(t, err, docbank.ErrBackupRepositoryLocked)
	once.Do(func() { close(resume) })
	require.NoError(t, <-done)
	reader, err := repository.OpenSnapshot(t.Context(), snapshot.ID)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
}

func TestBackupReaderRejectsInvalidSnapshots(t *testing.T) {
	for _, repository := range []*docbank.BackupRepository{nil, {}} {
		_, err := repository.OpenSnapshot(t.Context(), "snapshot")
		require.ErrorContains(t, err, "repository is required")
	}
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	for _, id := range []string{"", "../snapshot", "invalid"} {
		_, err = repository.OpenSnapshot(t.Context(), id)
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = repository.OpenSnapshot(ctx, "snapshot")
	require.ErrorIs(t, err, context.Canceled)
}
