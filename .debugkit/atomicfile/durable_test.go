//go:build unix || windows

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// injectSyncDir replaces directory sync with fake for the test.
func injectSyncDir(t *testing.T, fake func(string) error) {
	t.Helper()
	original := syncDir
	syncDir = fake
	t.Cleanup(func() { syncDir = original })
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestSyncFailureAfterPublishReportsNotDurable(t *testing.T) {
	for name, write := range map[string]func(string, []byte) error{
		"Commit": func(path string, data []byte) error {
			file, err := Create(path)
			if err != nil {
				return err
			}
			if _, err := file.Write(data); err != nil {
				return errors.Join(err, file.Abort())
			}
			return file.Commit()
		},
		"WriteNew": func(path string, data []byte) error { return WriteNew(path, data) },
	} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			dir := realTempDir(t)
			target := filepath.Join(dir, "target")
			cause := errors.New("injected sync failure")
			var synced []string
			injectSyncDir(t, func(d string) error {
				synced = append(synced, d)
				return cause
			})

			err := write(target, []byte("new"))

			require.ErrorIs(err, ErrNotDurable)
			require.ErrorIs(err, cause)
			assert.Equal(t, []string{dir}, synced)
			assert.Equal(t, "new", readFile(t, target))
			assert.Equal(t, []string{"target"}, dirEntries(t, dir))
		})
	}
}

func TestPublishSyncsDifferentStagingDir(t *testing.T) {
	for name, write := range map[string]func(string, []byte, ...Option) error{
		"WriteFile": WriteFile,
		"WriteNew":  WriteNew,
	} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			root := realTempDir(t)
			staging := filepath.Join(root, "staging")
			out := filepath.Join(root, "out")
			require.NoError(os.Mkdir(staging, 0o700))
			require.NoError(os.Mkdir(out, 0o700))
			target := filepath.Join(out, "target")
			cause := errors.New("injected staging sync failure")
			var synced []string
			injectSyncDir(t, func(d string) error {
				synced = append(synced, d)
				if d == staging {
					return cause
				}
				return nil
			})

			err := write(target, []byte("new"), WithStagingDir(staging))

			require.ErrorIs(err, ErrNotDurable)
			require.ErrorIs(err, cause)
			assert.ElementsMatch(t, []string{out, staging}, synced)
			assert.Equal(t, "new", readFile(t, target))
			assert.Empty(t, dirEntries(t, staging))
		})
	}
}

func TestPublishSyncsStagingDirOnceWhenSameAsTargetDir(t *testing.T) {
	require := require.New(t)
	dir := realTempDir(t)
	target := filepath.Join(dir, "target")
	var synced []string
	injectSyncDir(t, func(d string) error {
		synced = append(synced, d)
		return nil
	})

	require.NoError(WriteFile(target, []byte("new"), WithStagingDir(dir+string(filepath.Separator)+".")))

	assert.Equal(t, []string{dir}, synced)
	assert.Equal(t, "new", readFile(t, target))
}

// realTempDir returns t.TempDir() with its links resolved, the form atomicfile
// uses for the target's directory (on macOS /var is a link to /private/var).
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func TestWriteNewCleanupFailureIsPublishedButDurable(t *testing.T) {
	require := require.New(t)
	dir := realTempDir(t)
	target := filepath.Join(dir, "target")
	cause := errors.New("injected staging removal failure")
	original := removeFile
	removeFile = func(string) error { return cause }
	t.Cleanup(func() { removeFile = original })

	err := WriteNew(target, []byte("new"))

	require.ErrorIs(err, ErrPublished)
	require.ErrorIs(err, cause)
	require.NotErrorIs(err, ErrNotDurable)
	assert.Equal(t, "new", readFile(t, target))
}

func TestNotDurableIsPublished(t *testing.T) {
	require.ErrorIs(t, ErrNotDurable, ErrPublished)
}
