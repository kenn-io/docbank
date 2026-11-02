package backup_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
)

func TestForgetPortableParentKeepsSuccessorRestorable(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	source := &portableSource{
		raw:   []byte(`{"notes":["alpha"],"files":[]}`),
		stats: []byte(`{"notes":1,"files":0}`), info: &backup.ContentInfo{},
	}
	opts := backup.CreateOptions{MetadataSource: source, ContentDir: t.TempDir(), Jobs: 1}
	first, err := backup.Create(ctx, repo, portableApp{}, opts)
	require.NoError(err)
	source.raw = []byte(`{"notes":["beta"],"files":[]}`)
	second, err := backup.Create(ctx, repo, portableApp{}, opts)
	require.NoError(err)
	require.Equal(first.SnapshotID, second.ParentID)

	result, err := backup.Forget(ctx, repo, backup.ForgetOptions{SnapshotIDs: []string{first.SnapshotID}})
	require.NoError(err)
	require.Equal([]string{first.SnapshotID}, result.Forgotten)
	verified, err := backup.Verify(ctx, repo, portableApp{}, backup.VerifyOptions{All: true})
	require.NoError(err)
	require.Empty(verified.Problems)
	_, err = backup.Restore(ctx, repo, portableApp{}, backup.RestoreOptions{
		SnapshotID: second.SnapshotID, TargetDir: filepath.Join(t.TempDir(), "restored"),
		MetadataRestorer: portableRestorer{},
	})
	require.NoError(err)

	// A new backup must also work with an informational parent no longer present.
	source.raw = []byte(`{"notes":["gamma"],"files":[]}`)
	_, err = backup.Create(ctx, repo, portableApp{}, opts)
	require.NoError(err)
}
