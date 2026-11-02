package safefileio_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestValidatePrivateCurrentUserFileRejectsExtendedACL(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(os.WriteFile(path, []byte("{}"), 0o600))
	output, err := exec.CommandContext(t.Context(),
		"chmod",
		"+a",
		"everyone allow read",
		path,
	).CombinedOutput()
	require.NoError(err, string(output))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	defer func() { _ = file.Close() }()

	require.EqualError(safefileio.ValidatePrivateCurrentUserFile(file),
		"safefileio: file has a macOS extended ACL")
	listing, err := exec.CommandContext(t.Context(), "ls", "-lde", path).CombinedOutput()
	require.NoError(err, string(listing))
	assert.Contains(t, string(listing), "everyone allow read")
}

func TestValidatePrivateCurrentUserFileConcurrent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "private-record")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	require.NoError(t, safefileio.ValidatePrivateCurrentUserFile(file))

	// Resuming Go after the native ACL call can change thread-local errno.
	// All readers share an unchanged private file, so every validation must pass.
	var validators sync.WaitGroup
	var failures atomic.Int64
	var firstFailure sync.Once
	var firstErr error
	for range 16 {
		validators.Go(func() {
			for range 5000 {
				if err := safefileio.ValidatePrivateCurrentUserFile(file); err != nil {
					failures.Add(1)
					firstFailure.Do(func() { firstErr = err })
				}
			}
		})
	}
	validators.Wait()
	require.NoError(t, firstErr, "%d validations rejected an unchanged private file", failures.Load())
}
