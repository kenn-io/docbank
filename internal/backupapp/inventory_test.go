package backupapp

import (
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
