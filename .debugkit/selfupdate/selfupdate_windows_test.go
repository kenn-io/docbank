package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestInstallBinaryBackupSharingViolationPersists(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "source.exe")
		dst := filepath.Join(dir, "tool.exe")
		require.NoError(t, os.WriteFile(src, []byte("new"), 0o755))
		require.NoError(t, os.WriteFile(dst, []byte("original"), 0o755))

		handle, err := os.Open(dst)
		require.NoError(t, err)
		defer func() { assert.NoError(t, handle.Close()) }()

		err = InstallBinary(src, dst)
		require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
		var renameErr *os.LinkError
		require.ErrorAs(t, err, &renameErr)
		assert.Equal(t, dst, renameErr.Old)
		require.ErrorContains(t, err, "backup: ")
		data, err := os.ReadFile(dst)
		require.NoError(t, err)
		assert.Equal(t, "original", string(data))
		staging, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*"))
		require.NoError(t, err)
		assert.Empty(t, staging)
	})
}

func TestInstallBinaryWaitsForRollbackReader(t *testing.T) {
	for _, tt := range []struct {
		name         string
		rollback     bool
		releaseAfter time.Duration
		want         string
	}{
		{name: "staged reader released", releaseAfter: 100 * time.Millisecond, want: "new"},
		{name: "rollback reader released", rollback: true, releaseAfter: 2300 * time.Millisecond, want: "original"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				src := filepath.Join(dir, "source.exe")
				dst := filepath.Join(dir, "tool.exe")
				require.NoError(t, os.WriteFile(src, []byte("new"), 0o755))
				require.NoError(t, os.WriteFile(dst, []byte("original"), 0o755))

				// Block the backup move until the staged file has a reader.
				destination, err := os.Open(dst)
				require.NoError(t, err)
				defer func() { _ = destination.Close() }()
				installed := make(chan error, 1)
				go func() { installed <- InstallBinary(src, dst) }()
				synctest.Wait()
				staged, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*.new"))
				require.NoError(t, err)
				require.Len(t, staged, 1)
				reader, err := os.Open(staged[0])
				require.NoError(t, err)
				defer func() { _ = reader.Close() }()
				require.NoError(t, destination.Close())

				var backup *os.File
				if tt.rollback {
					var backups []string
					require.Eventually(t, func() bool {
						paths, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*.old"))
						backups = paths
						_, statErr := os.Stat(dst)
						return err == nil && len(backups) == 1 && os.IsNotExist(statErr)
					}, time.Second, 5*time.Millisecond)
					// The renamed path is visible before Replace closes its handle.
					// Wait for the installer to reach its next retry before opening it.
					synctest.Wait()
					backup, err = os.Open(backups[0])
					require.NoError(t, err)
					defer func() { _ = backup.Close() }()
				}

				// Virtual time keeps readers held through install or rollback retries.
				time.Sleep(tt.releaseAfter)
				require.NoError(t, reader.Close())
				if backup != nil {
					require.NoError(t, backup.Close())
				}
				err = <-installed
				if tt.rollback {
					require.Error(t, err)
					require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
					require.ErrorContains(t, err, "install: ")
					assert.NotContains(t, err.Error(), "rollback also failed")
				} else {
					require.NoError(t, err)
				}
				data, err := os.ReadFile(dst)
				require.NoError(t, err)
				assert.Equal(t, tt.want, string(data))
				staging, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*"))
				require.NoError(t, err)
				assert.Empty(t, staging)
			})
		})
	}
}
