//go:build windows

package packstore

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestReplaceLooseRepairFileWindowsReplacesActiveReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	before := []byte("old open descriptor")
	after := []byte("verified replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	require.NoError(os.WriteFile(final, before, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)
	active, err := openNoFollow(final, false)
	require.NoError(err)
	t.Cleanup(func() { _ = active.Close() })

	result, err := replaceLooseRepairFile(staging, final, verified)

	require.NoError(err)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal(after, mustReadFile(t, final))
	oldBytes, err := io.ReadAll(active)
	require.NoError(err)
	assert.Equal(before, oldBytes)
}

func TestReplaceLooseRepairFileWindowsReplacesWhileProductionPinIsHeld(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	before := []byte("old canonical bytes")
	after := []byte("verified pinned replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	require.NoError(os.WriteFile(final, before, 0o600))
	pin, identity, err := openLooseRepairPin(staging)
	require.NoError(err)
	t.Cleanup(func() { _ = pin.Close() })

	result, err := replaceLooseRepairFile(staging, final, identity)

	require.NoError(err)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal(after, mustReadFile(t, final))
	canonicalIdentity, err := os.Stat(final)
	require.NoError(err)
	assert.True(os.SameFile(identity, canonicalIdentity), "canonical must have the verified pinned identity")
}

func TestReplaceLooseRepairFileWindowsCreatesAbsentTargetWithoutClobber(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	content := []byte("verified absent-target repair")
	require.NoError(os.WriteFile(staging, content, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)

	result, err := replaceLooseRepairFile(staging, final, verified)

	require.NoError(err)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal(content, mustReadFile(t, final))
}

func TestReplaceLooseRepairFileWindowsFallsBackWhenHardLinksAreUnsupported(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	content := []byte("verified repair without hard links")
	require.NoError(os.WriteFile(staging, content, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)
	missing := &fs.PathError{Op: "ReplaceFileW", Path: final, Err: fs.ErrNotExist}
	originalReplace := replaceLooseFileWindows
	originalLink := linkLooseRepairFileWindows
	replaceLooseFileWindows = func(string, string, string) error { return missing }
	linkLooseRepairFileWindows = func(string, string) error { return errors.ErrUnsupported }
	t.Cleanup(func() {
		replaceLooseFileWindows = originalReplace
		linkLooseRepairFileWindows = originalLink
	})

	result, err := replaceLooseRepairFile(staging, final, verified)

	require.NoError(err)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.NoFileExists(staging)
	assert.Equal(content, mustReadFile(t, final))
}

func TestReplaceLooseRepairFileWindowsRetriesReplaceAfterFallbackMoveRace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	content := []byte("verified fallback race replacement")
	require.NoError(os.WriteFile(staging, content, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)
	missing := &fs.PathError{Op: "ReplaceFileW", Path: final, Err: fs.ErrNotExist}
	originalReplace := replaceLooseFileWindows
	originalLink := linkLooseRepairFileWindows
	var calls []string
	replaceLooseFileWindows = func(gotStaging, gotFinal, backup string) error {
		calls = append(calls, "replace")
		require.Equal(staging, gotStaging)
		require.Equal(final, gotFinal)
		if len(calls) == 1 {
			return missing
		}
		require.NoError(os.Rename(final, backup))
		require.NoError(os.Rename(staging, final))
		return nil
	}
	linkLooseRepairFileWindows = func(gotStaging, gotFinal string) error {
		calls = append(calls, "link")
		require.Equal(staging, gotStaging)
		require.Equal(final, gotFinal)
		require.NoError(os.WriteFile(final, []byte("racing canonical"), 0o600))
		return errors.ErrUnsupported
	}
	t.Cleanup(func() {
		replaceLooseFileWindows = originalReplace
		linkLooseRepairFileWindows = originalLink
	})

	result, err := replaceLooseRepairFile(staging, final, verified)

	require.NoError(err)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal([]string{"replace", "link", "replace"}, calls)
	assert.Equal(content, mustReadFile(t, final))
}

func TestReplaceLooseRepairFileWindowsRetriesReplaceAfterCreateRace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	require.NoError(os.WriteFile(staging, []byte("verified race replacement"), 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)
	missing := &fs.PathError{Op: "ReplaceFileW", Path: final, Err: fs.ErrNotExist}
	originalReplace := replaceLooseFileWindows
	originalLink := linkLooseRepairFileWindows
	var calls []string
	replaceLooseFileWindows = func(staging, final, backup string) error {
		calls = append(calls, "replace:"+staging+":"+final+":"+filepath.Dir(backup))
		if len(calls) == 1 {
			return missing
		}
		require.NoError(os.Rename(final, backup))
		require.NoError(os.Rename(staging, final))
		return nil
	}
	linkLooseRepairFileWindows = func(staging, final string) error {
		calls = append(calls, "link:"+staging+":"+final)
		require.NoError(os.WriteFile(final, []byte("racing canonical"), 0o600))
		return fs.ErrExist
	}
	t.Cleanup(func() {
		replaceLooseFileWindows = originalReplace
		linkLooseRepairFileWindows = originalLink
	})

	result, err := replaceLooseRepairFile(staging, final, verified)

	require.NoError(err)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal([]byte("verified race replacement"), mustReadFile(t, final))
	assert.Equal([]string{
		"replace:" + staging + ":" + final + ":" + dir,
		"link:" + staging + ":" + final,
		"replace:" + staging + ":" + final + ":" + dir,
	}, calls)
}

func TestReplaceLooseRepairFileWindowsReconcilesDocumentedPartialFailures(t *testing.T) {
	for _, tt := range []struct {
		name        string
		code        error
		moveOld     bool
		wantCreated bool
		wantKeep    bool
		wantShard   bool
		wantStaging bool
		wantFinal   []byte
	}{
		{
			name:        "unable to remove replaced",
			code:        windows.ERROR_UNABLE_TO_REMOVE_REPLACED,
			wantKeep:    true,
			wantStaging: true,
			wantFinal:   []byte("old canonical evidence"),
		},
		{
			name:        "unable to move replacement",
			code:        windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT,
			wantKeep:    true,
			wantStaging: true,
			wantFinal:   []byte("old canonical evidence"),
		},
		{
			name:        "unable to move replacement after backup",
			code:        windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT_2,
			moveOld:     true,
			wantCreated: true,
			wantShard:   true,
			wantFinal:   []byte("verified replacement"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			staging := filepath.Join(dir, "staging")
			final := filepath.Join(dir, "final")
			require.NoError(os.WriteFile(staging, []byte("verified replacement"), 0o600))
			require.NoError(os.WriteFile(final, []byte("old canonical evidence"), 0o600))
			verified, err := os.Stat(staging)
			require.NoError(err)
			originalReplace := replaceLooseFileWindows
			var backup string
			replaceLooseFileWindows = func(gotStaging, gotFinal, gotBackup string) error {
				require.Equal(staging, gotStaging)
				require.Equal(final, gotFinal)
				require.Equal(dir, filepath.Dir(gotBackup))
				require.NotEqual(staging, gotBackup)
				require.NotEqual(final, gotBackup)
				backup = gotBackup
				if tt.moveOld {
					require.NoError(os.Rename(final, backup))
				}
				return tt.code
			}
			t.Cleanup(func() { replaceLooseFileWindows = originalReplace })

			result, err := replaceLooseRepairFile(staging, final, verified)

			require.ErrorIs(err, tt.code)
			assert.Equal(tt.wantCreated, result.Created)
			assert.Equal(tt.wantKeep, result.KeepStaging)
			assert.Equal(tt.wantShard, result.SyncShard)
			assert.Equal(tt.wantStaging, result.SyncStaging)
			assert.Equal(tt.wantFinal, mustReadFile(t, final))
			assert.NotEmpty(backup)
			assert.NoFileExists(backup)
			if tt.wantKeep {
				assert.Equal([]byte("verified replacement"), mustReadFile(t, staging))
			}
		})
	}
}
