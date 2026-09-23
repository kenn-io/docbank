package filepublish

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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
