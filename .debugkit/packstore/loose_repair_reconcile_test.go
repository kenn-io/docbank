package packstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errInjectedReplaceFailure = errors.New("injected replacement failure")
	errInjectedMoveFailure    = errors.New("injected replacement move failure")
	errInjectedRecoveryState  = errors.New("injected replacement recovery state")
)

func TestReconcileLooseRepairReplacementPreservesVerifiedStagingWhenNamesAreUnchanged(t *testing.T) {
	for _, code := range []error{errInjectedReplaceFailure, errInjectedMoveFailure} {
		t.Run(code.Error(), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			staging := filepath.Join(dir, "staging")
			final := filepath.Join(dir, "final")
			backup := filepath.Join(dir, "backup")
			before := []byte("old canonical evidence")
			after := []byte("verified replacement")
			require.NoError(os.WriteFile(staging, after, 0o600))
			require.NoError(os.WriteFile(final, before, 0o600))
			verified, err := os.Stat(staging)
			require.NoError(err)

			result, err := reconcileLooseRepairReplacement(staging, final, backup, verified, code)

			require.ErrorIs(err, code)
			assert.False(result.Created)
			assert.True(result.KeepStaging)
			assert.False(result.SyncShard)
			assert.True(result.SyncStaging)
			assert.Equal(before, mustReadFile(t, final))
			assert.Equal(after, mustReadFile(t, staging))
			assert.NoFileExists(backup)
		})
	}
}

func TestReconcileLooseRepairReplacementPublishesVerifiedStagingAfterMoveFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	backup := filepath.Join(dir, "backup")
	before := []byte("old canonical backup")
	after := []byte("verified replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	require.NoError(os.WriteFile(backup, before, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)

	result, err := reconcileLooseRepairReplacement(staging, final, backup, verified, errInjectedRecoveryState)

	require.ErrorIs(err, errInjectedRecoveryState)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal(after, mustReadFile(t, final))
	assert.Equal(after, mustReadFile(t, staging))
	assert.NoFileExists(backup)
}

func TestReconcileLooseRepairReplacementFallsBackWhenHardLinksAreUnsupported(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	backup := filepath.Join(dir, "backup")
	content := []byte("verified reconciliation without hard links")
	require.NoError(os.WriteFile(staging, content, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)
	originalLink := linkLooseRepairRecoveryFile
	linkLooseRepairRecoveryFile = func(string, string) error { return errors.ErrUnsupported }
	t.Cleanup(func() { linkLooseRepairRecoveryFile = originalLink })

	result, err := reconcileLooseRepairReplacement(staging, final, backup, verified, errInjectedRecoveryState)

	require.ErrorIs(err, errInjectedRecoveryState)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.NoFileExists(staging)
	assert.Equal(content, mustReadFile(t, final))
}

func TestReconcileLooseRepairReplacementRecognizesReplacementDespiteAPIError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	backup := filepath.Join(dir, "backup")
	before := []byte("old canonical backup")
	after := []byte("verified replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	pin, verified, err := openLooseRepairPin(staging)
	require.NoError(err)
	t.Cleanup(func() { _ = pin.Close() })
	require.NoError(os.WriteFile(backup, before, 0o600))
	require.NoError(os.Rename(staging, final))

	result, err := reconcileLooseRepairReplacement(staging, final, backup, verified, errInjectedReplaceFailure)

	require.ErrorIs(err, errInjectedReplaceFailure)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal(after, mustReadFile(t, final))
	assert.NoFileExists(staging)
	assert.NoFileExists(backup)
}

func TestReconcileLooseRepairReplacementRestoresOnlyBackup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	backup := filepath.Join(dir, "backup")
	before := []byte("old canonical backup")
	after := []byte("verified replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)
	require.NoError(os.Remove(staging))
	require.NoError(os.WriteFile(backup, before, 0o600))

	result, err := reconcileLooseRepairReplacement(staging, final, backup, verified, errInjectedRecoveryState)

	require.ErrorIs(err, errInjectedRecoveryState)
	assert.False(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal(before, mustReadFile(t, final))
	assert.NoFileExists(staging)
	assert.NoFileExists(backup)
}

func TestReconcileLooseRepairReplacementPreservesOnlyBackupWhenRestoreFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	backup := filepath.Join(dir, "backup")
	before := []byte("only remaining old canonical")
	after := []byte("lost verified replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)
	require.NoError(os.Remove(staging))
	require.NoError(os.WriteFile(backup, before, 0o600))
	recoveryErr := errors.New("injected backup restoration failure")
	originalLink := linkLooseRepairRecoveryFile
	originalRename := renameLooseRepairRecoveryFile
	linkLooseRepairRecoveryFile = func(oldname, newname string) error {
		require.Equal(backup, oldname)
		require.Equal(final, newname)
		return recoveryErr
	}
	renameLooseRepairRecoveryFile = func(oldname, newname string) error {
		require.Equal(backup, oldname)
		require.Equal(final, newname)
		return recoveryErr
	}
	t.Cleanup(func() {
		linkLooseRepairRecoveryFile = originalLink
		renameLooseRepairRecoveryFile = originalRename
	})

	result, err := reconcileLooseRepairReplacement(staging, final, backup, verified, errInjectedRecoveryState)

	require.ErrorIs(err, errInjectedRecoveryState)
	require.ErrorIs(err, recoveryErr)
	assert.False(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal(before, mustReadFile(t, backup))
	assert.NoFileExists(staging)
	assert.NoFileExists(final)
}

func TestReconcileLooseRepairReplacementAcceptsNoClobberRecoveryRace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	backup := filepath.Join(dir, "backup")
	after := []byte("verified raced replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)
	originalLink := linkLooseRepairRecoveryFile
	linkLooseRepairRecoveryFile = func(oldname, newname string) error {
		require.Equal(staging, oldname)
		require.Equal(final, newname)
		require.NoError(os.Link(oldname, newname))
		return fs.ErrExist
	}
	t.Cleanup(func() { linkLooseRepairRecoveryFile = originalLink })

	result, err := reconcileLooseRepairReplacement(staging, final, backup, verified, errInjectedRecoveryState)

	require.ErrorIs(err, errInjectedRecoveryState)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal(after, mustReadFile(t, final))
}

func TestReconcileLooseRepairReplacementReportsBackupCleanupAfterPartialSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	backup := filepath.Join(dir, "backup")
	before := []byte("old canonical backup")
	after := []byte("verified replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	pin, verified, err := openLooseRepairPin(staging)
	require.NoError(err)
	t.Cleanup(func() { _ = pin.Close() })
	require.NoError(os.WriteFile(backup, before, 0o600))
	require.NoError(os.Rename(staging, final))
	cleanupErr := errors.New("injected repair backup cleanup failure")
	originalRemove := removeLooseRepairBackupFile
	removeLooseRepairBackupFile = func(path string) error {
		require.Equal(backup, path)
		return cleanupErr
	}
	t.Cleanup(func() { removeLooseRepairBackupFile = originalRemove })

	result, err := reconcileLooseRepairReplacement(staging, final, backup, verified, errInjectedReplaceFailure)

	require.ErrorIs(err, errInjectedReplaceFailure)
	require.ErrorIs(err, cleanupErr)
	assert.True(result.Created)
	assert.False(result.KeepStaging)
	assert.True(result.SyncShard)
	assert.False(result.SyncStaging)
	assert.Equal(after, mustReadFile(t, final))
	assert.Equal(before, mustReadFile(t, backup))
}

func TestReconcileLooseRepairReplacementKeepsLastVerifiedCopyWhenRecoveryFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	backup := filepath.Join(dir, "backup")
	after := []byte("last verified replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	verified, err := os.Stat(staging)
	require.NoError(err)
	recoveryErr := errors.New("injected no-clobber recovery failure")
	originalLink := linkLooseRepairRecoveryFile
	originalRename := renameLooseRepairRecoveryFile
	linkLooseRepairRecoveryFile = func(oldname, newname string) error {
		require.Equal(staging, oldname)
		require.Equal(final, newname)
		return recoveryErr
	}
	renameLooseRepairRecoveryFile = func(oldname, newname string) error {
		require.Equal(staging, oldname)
		require.Equal(final, newname)
		return recoveryErr
	}
	t.Cleanup(func() {
		linkLooseRepairRecoveryFile = originalLink
		renameLooseRepairRecoveryFile = originalRename
	})

	result, err := reconcileLooseRepairReplacement(staging, final, backup, verified, errInjectedRecoveryState)

	require.ErrorIs(err, errInjectedRecoveryState)
	require.ErrorIs(err, recoveryErr)
	assert.False(result.Created)
	assert.True(result.KeepStaging)
	assert.False(result.SyncShard)
	assert.True(result.SyncStaging)
	assert.Equal(after, mustReadFile(t, staging))
	assert.NoFileExists(final)
	assert.NoFileExists(backup)
}
