package backupapp

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestReadSnapshotExtraFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "catalog.sqlite")
	want := []byte("verified archive extra")
	require.NoError(t, os.WriteFile(source, want, 0o600))
	repository, err := backup.Init(filepath.Join(root, "repository"))
	require.NoError(t, err)
	known, err := repository.LoadBlobIndex()
	require.NoError(t, err)
	appender := backup.NewPackAppender(repository, known, pack.DefaultZstdLevel, nil, packstore.PackExt)
	treeID, hasTree, err := backup.CaptureExtras(t.Context(), backup.ExtrasOptions{
		Spec: backup.ExtrasSpec{Files: []backup.ExtrasFileSpec{{Path: source, RecordAs: "application/catalog.sqlite"}}},
	}, appender)
	require.NoError(t, err)
	require.True(t, hasTree)
	packs, entries, err := appender.Finish()
	require.NoError(t, err)
	indexID, err := repository.WriteIndex(entries)
	require.NoError(t, err)
	snapshotID, err := repository.WriteManifest(&backup.Manifest{
		FormatVersion: 4, MinReaderVersion: 4, AppVersion: "backupapp-test",
		CreatedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Extras:    backup.ManifestExtras{Tree: treeID.String()}, NewPacks: packs, NewIndex: indexID,
	})
	require.NoError(t, err)

	destination := filepath.Join(root, "out", "catalog.sqlite")
	require.NoError(t, ReadSnapshotExtraFile(t.Context(), repository, snapshotID, "application/catalog.sqlite", destination))
	got, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, want, got)
	_, err = os.Stat(filepath.Join(root, "out", "other.sqlite"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestReadSnapshotExtraFileRejectsDuplicateRequestedPath(t *testing.T) {
	root := t.TempDir()
	repository, err := backup.Init(filepath.Join(root, "repository"))
	require.NoError(t, err)
	known, err := repository.LoadBlobIndex()
	require.NoError(t, err)
	appender := backup.NewPackAppender(repository, known, pack.DefaultZstdLevel, nil, packstore.PackExt)
	firstID, _, err := appender.Add([]byte("first catalog"))
	require.NoError(t, err)
	secondID, _, err := appender.Add([]byte("second catalog"))
	require.NoError(t, err)
	tree, err := json.Marshal(backup.ExtrasTree{Entries: []backup.ExtrasEntry{
		{Path: "application/catalog.sqlite", Mode: 0o600, Size: int64(len("first catalog")), Blob: firstID.String()},
		{Path: "application/catalog.sqlite", Mode: 0o600, Size: int64(len("second catalog")), Blob: secondID.String()},
	}})
	require.NoError(t, err)
	treeID, _, err := appender.Add(tree)
	require.NoError(t, err)
	packs, entries, err := appender.Finish()
	require.NoError(t, err)
	indexID, err := repository.WriteIndex(entries)
	require.NoError(t, err)
	snapshotID, err := repository.WriteManifest(&backup.Manifest{
		FormatVersion: 4, MinReaderVersion: 4, AppVersion: "backupapp-test",
		CreatedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Extras:    backup.ManifestExtras{Tree: treeID.String()}, NewPacks: packs, NewIndex: indexID,
	})
	require.NoError(t, err)

	destination := filepath.Join(root, "out", "catalog.sqlite")
	err = ReadSnapshotExtraFile(t.Context(), repository, snapshotID, "application/catalog.sqlite", destination)
	require.ErrorContains(t, err, "appears more than once")
	_, statErr := os.Stat(destination)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestReadSnapshotExtraFileRejectsOversizedTree(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "out", "catalog.sqlite")
	repository, snapshotID := snapshotWithExtrasTree(t, func(string) []byte {
		return bytes.Repeat([]byte{' '}, int(packstore.DefaultLimits().BlobBytes+1))
	}, []byte("catalog"))

	err := ReadSnapshotExtraFile(t.Context(), repository, snapshotID, "application/catalog.sqlite", destination)
	require.ErrorContains(t, err, "extras tree size")
	_, statErr := os.Stat(destination)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestReadSnapshotExtraFileAcceptsExactLimitTree(t *testing.T) {
	want := []byte("exact-limit archive extra")
	destination := filepath.Join(t.TempDir(), "out", "catalog.sqlite")
	repository, snapshotID := snapshotWithExtrasTree(t, func(blob string) []byte {
		tree, err := json.Marshal(backup.ExtrasTree{Entries: []backup.ExtrasEntry{{
			Path: "application/catalog.sqlite", Mode: 0o600, Size: int64(len(want)), Blob: blob,
		}}})
		require.NoError(t, err)
		maxExtrasTreeBytes := packstore.DefaultLimits().BlobBytes
		require.LessOrEqual(t, len(tree), int(maxExtrasTreeBytes))
		return append(tree, bytes.Repeat([]byte{' '}, int(maxExtrasTreeBytes)-len(tree))...)
	}, want)

	require.NoError(t, ReadSnapshotExtraFile(t.Context(), repository, snapshotID, "application/catalog.sqlite", destination))
	got, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestReadSnapshotExtraFileRejectsMalformedTree(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "out", "catalog.sqlite")
	repository, snapshotID := snapshotWithExtrasTree(t, func(string) []byte {
		return []byte(`{"entries":[`)
	}, []byte("catalog"))

	err := ReadSnapshotExtraFile(t.Context(), repository, snapshotID, "application/catalog.sqlite", destination)
	require.ErrorContains(t, err, "read extras tree")
	_, statErr := os.Stat(destination)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestReadSnapshotExtraFileRejectsTrailingTreeValue(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "out", "catalog.sqlite")
	want := []byte("trailing-value archive extra")
	repository, snapshotID := snapshotWithExtrasTree(t, func(blob string) []byte {
		tree, err := json.Marshal(backup.ExtrasTree{Entries: []backup.ExtrasEntry{{
			Path: "application/catalog.sqlite", Mode: 0o600, Size: int64(len(want)), Blob: blob,
		}}})
		require.NoError(t, err)
		return append(tree, []byte(` {}`)...)
	}, want)

	err := ReadSnapshotExtraFile(t.Context(), repository, snapshotID, "application/catalog.sqlite", destination)
	require.ErrorContains(t, err, "read extras tree")
	_, statErr := os.Stat(destination)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func snapshotWithExtrasTree(t *testing.T, tree func(blob string) []byte, extra []byte) (*backup.Repo, string) {
	t.Helper()
	repository, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	known, err := repository.LoadBlobIndex()
	require.NoError(t, err)
	appender := backup.NewPackAppender(repository, known, pack.DefaultZstdLevel, nil, packstore.PackExt)
	extraID, _, err := appender.Add(extra)
	require.NoError(t, err)
	treeID, _, err := appender.Add(tree(extraID.String()))
	require.NoError(t, err)
	packs, entries, err := appender.Finish()
	require.NoError(t, err)
	indexID, err := repository.WriteIndex(entries)
	require.NoError(t, err)
	snapshotID, err := repository.WriteManifest(&backup.Manifest{
		FormatVersion: 4, MinReaderVersion: 4, AppVersion: "backupapp-test",
		CreatedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Extras:    backup.ManifestExtras{Tree: treeID.String()}, NewPacks: packs, NewIndex: indexID,
	})
	require.NoError(t, err)
	return repository, snapshotID
}
