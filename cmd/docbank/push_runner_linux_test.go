//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Linux filesystems accept filenames that are not UTF-8; Docbank names are.
func TestPushReportsANonUTF8FilenameAndContinues(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	const invalid = "bad\xff.txt"
	require.NoError(t, os.WriteFile(filepath.Join(dir, invalid), []byte("unnamed"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "good.txt"), []byte("named"), 0o600))
	opts := options(dir)
	var failed []string
	opts.Failure = func(ref string, err error) error {
		failed = append(failed, ref)
		assert.ErrorContains(t, err, "not valid UTF-8")
		return nil
	}
	report, err := executePush(t.Context(), f.connection, opts)
	require.ErrorIs(t, err, errPushFilesFailed)
	assert.Equal(t, 1, report.Added)
	assert.Equal(t, 1, report.Failed)
	assert.Equal(t, []string{invalid}, failed)
}
