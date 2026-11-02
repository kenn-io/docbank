package backup_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
)

func TestPrunePreservesPortableMetadataAuxiliaryContentAndExtras(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	content := []byte("shared retained attachment")
	hash := pack.ComputeBlobID(content).String()
	contentDir := t.TempDir()
	require.NoError(os.Mkdir(filepath.Join(contentDir, hash[:2]), 0o700))
	require.NoError(os.WriteFile(filepath.Join(contentDir, hash[:2], hash), content, 0o600))
	placement := []byte(`{"stores":["archive"]}`)
	source := &portableSource{
		stats: []byte(`{"notes":1,"files":1}`),
		info:  &backup.ContentInfo{Refs: []backup.ContentRef{{Hash: hash, Size: int64(len(content))}}, Rows: 1},
		auxiliary: []backup.AuxiliaryArtifact{{
			Name: "placement", Format: "synthetic-placement-v1",
			Open: func(context.Context) (io.ReadCloser, int64, error) {
				return io.NopCloser(bytes.NewReader(placement)), int64(len(placement)), nil
			},
		}},
	}
	extraPath := filepath.Join(t.TempDir(), "extra")
	unused := make([]byte, 256<<10)
	_, err = rand.Read(unused)
	require.NoError(err)
	require.NoError(os.WriteFile(extraPath, unused, 0o600))
	opts := backup.CreateOptions{
		MetadataSource: source, ContentDir: contentDir, Jobs: 1,
		Extras: backup.ExtrasSpec{Files: []backup.ExtrasFileSpec{{Path: extraPath, RecordAs: "catalog-snapshot"}}},
	}
	record := portableRecord{Notes: []string{"old"}, Files: []portableFile{{Hash: hash, Size: int64(len(content)), Path: hash[:2] + "/" + hash}}}
	source.raw, err = json.Marshal(record)
	require.NoError(err)
	first, err := backup.Create(t.Context(), repo, portableApp{}, opts)
	require.NoError(err)
	record.Notes = []string{"retained"}
	source.raw, err = json.Marshal(record)
	require.NoError(err)
	require.NoError(os.WriteFile(extraPath, []byte("retained extra"), 0o600))
	second, err := backup.Create(t.Context(), repo, portableApp{}, opts)
	require.NoError(err)
	_, err = backup.Forget(t.Context(), repo, backup.ForgetOptions{SnapshotIDs: []string{first.SnapshotID}})
	require.NoError(err)
	result, err := backup.Prune(t.Context(), repo, portableApp{}, backup.PruneOptions{})
	require.NoError(err)
	require.NotEmpty(result.PacksToRepack)
	require.Greater(result.BytesRemoved, result.BytesWritten)
	verified, err := backup.Verify(t.Context(), repo, portableApp{}, backup.VerifyOptions{All: true})
	require.NoError(err)
	require.Empty(verified.Problems)
	target := filepath.Join(t.TempDir(), "restore")
	var restoredAuxiliary []backup.RestoredAuxiliary
	_, err = backup.Restore(t.Context(), repo, portableApp{}, backup.RestoreOptions{
		SnapshotID: second.SnapshotID, TargetDir: target, MetadataRestorer: portableRestorer{},
		AuxiliaryTarget: auxiliaryTargetFunc(func(_ context.Context, artifacts []backup.RestoredAuxiliary) (backup.AuxiliaryRestore, error) {
			restoredAuxiliary = artifacts
			return auxiliaryRestoreFuncs{
				commit:   func(context.Context) error { return nil },
				rollback: func(context.Context) error { return nil },
			}, nil
		}),
	})
	require.NoError(err)
	require.Len(restoredAuxiliary, 1)
	assert.Equal(placement, restoredAuxiliary[0].Data)
	got, err := os.ReadFile(filepath.Join(target, "catalog-snapshot"))
	require.NoError(err)
	assert.Equal("retained extra", string(got))
	got, err = os.ReadFile(filepath.Join(target, "content", hash[:2], hash))
	require.NoError(err)
	assert.Equal(content, got)
}
