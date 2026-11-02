//go:build darwin || linux || windows

package atomicfile

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func forceLinkFailure(t *testing.T) {
	t.Helper()
	original := linkFile
	linkFile = func(string, string) error { return fs.ErrInvalid }
	t.Cleanup(func() { linkFile = original })
}

func TestPublishNoReplaceFallbackNeverReplacesExistingDestination(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	require.NoError(os.WriteFile(staging, []byte("new staged content"), 0o600))
	require.NoError(os.WriteFile(final, []byte("existing canonical content"), 0o600))
	forceLinkFailure(t)

	err := PublishNoReplace(staging, final)

	require.ErrorIs(err, fs.ErrExist)
	require.ErrorIs(err, fs.ErrInvalid)
	staged, err := os.ReadFile(staging)
	require.NoError(err)
	assert.Equal("new staged content", string(staged))
	existing, err := os.ReadFile(final)
	require.NoError(err)
	assert.Equal("existing canonical content", string(existing))
}

func TestPublishNoReplaceFallbackRenamesWhenLinkFails(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	require.NoError(os.WriteFile(staging, []byte("staged"), 0o600))
	forceLinkFailure(t)

	require.NoError(PublishNoReplace(staging, final))

	data, err := os.ReadFile(final)
	require.NoError(err)
	assert.Equal(t, "staged", string(data))
	assert.NoFileExists(t, staging)
}

func TestPublishNoReplaceHardLinkLeavesStaging(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	require.NoError(os.WriteFile(staging, []byte("staged"), 0o600))

	require.NoError(PublishNoReplace(staging, final))

	data, err := os.ReadFile(final)
	require.NoError(err)
	assert.Equal(t, "staged", string(data))
	assert.FileExists(t, staging)
}
