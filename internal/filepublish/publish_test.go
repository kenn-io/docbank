package filepublish

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublishHonorsNoReplaceAndReportsPostInstallDurability(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "out.pdf")
	stage := func(name, value string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
		return path
	}
	published, err := Publish(stage("first.tmp", "first"), destination, false)
	require.NoError(t, err)
	require.True(t, published)
	published, err = Publish(stage("second.tmp", "second"), destination, false)
	require.Error(t, err)
	require.False(t, published)
	got, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "first", string(got))

	previous := syncDirectory
	syncDirectory = func(string) error { return errors.New("synthetic sync failure") }
	t.Cleanup(func() { syncDirectory = previous })
	published, err = Publish(stage("replace.tmp", "replacement"), destination, true)
	require.ErrorContains(t, err, "directory sync")
	require.True(t, published)
	got, err = os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "replacement", string(got))
}

func TestCreateStageRemovesOnlyStaleAbandonedStages(t *testing.T) {
	parent := t.TempDir()
	stale := time.Now().Add(-2 * staleStageAge)
	abandoned, err := CreateStage(parent, ".docbank-test-")
	require.NoError(t, err)
	require.NoError(t, abandoned.File.Close())
	if abandoned.pin != nil {
		// A crashed process no longer holds its Windows directory pin.
		require.NoError(t, abandoned.pin.Close())
	}
	require.NoError(t, os.Chtimes(abandoned.dir, stale, stale))
	live, err := CreateStage(parent, ".docbank-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Cleanup() })
	userDir := filepath.Join(parent, ".docbank-test-user")
	require.NoError(t, os.Mkdir(userDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(userDir, "notes.txt"), []byte("keep"), 0o600))
	require.NoError(t, os.Chtimes(userDir, stale, stale))

	next, err := CreateStage(parent, ".docbank-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = next.Cleanup() })

	require.NoDirExists(t, abandoned.dir)
	require.DirExists(t, live.dir)
	require.FileExists(t, filepath.Join(userDir, "notes.txt"))
}
