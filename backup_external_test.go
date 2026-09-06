package docbank_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank"
)

func TestEmbeddedBackupCapturesHostFilePreparedInsideFreeze(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(t.TempDir(), "live")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	createRangeFixture(t, vault, "/archive/photo.txt", []byte("snapshot content\n"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "backups"))
	require.NoError(t, err)

	extraPath := filepath.Join(t.TempDir(), "catalog.sqlite")
	prepared := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	t.Cleanup(func() { resumeOnce.Do(func() { close(resume) }) })
	type backupResult struct {
		snapshot docbank.BackupSnapshot
		err      error
	}
	backupDone := make(chan backupResult, 1)
	go func() {
		snapshot, createErr := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
			AllowPlaintextSecrets: true,
			Prepare: func(ctx context.Context) error {
				close(prepared)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-resume:
				}
				return os.WriteFile(extraPath, []byte("host catalog\n"), 0o600)
			},
			ExtraFiles: []docbank.BackupExtraFile{{
				Path: extraPath, RecordAs: "host/catalog.sqlite", Sensitive: true,
			}},
		})
		backupDone <- backupResult{snapshot: snapshot, err: createErr}
	}()
	select {
	case <-prepared:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "backup preparation did not begin")
	}

	putDone := make(chan error, 1)
	go func() {
		_, putErr := vault.Put(t.Context(), "/archive/later.txt", bytes.NewBufferString("later\n"), docbank.PutOptions{
			MediaType: "text/plain",
		})
		putDone <- putErr
	}()
	select {
	case putErr := <-putDone:
		require.FailNow(t, "content mutation completed during host preparation", "error: %v", putErr)
	case <-time.After(100 * time.Millisecond):
	}

	resumeOnce.Do(func() { close(resume) })
	result := <-backupDone
	require.NoError(t, result.err)
	require.NoError(t, <-putDone)

	target := filepath.Join(t.TempDir(), "restored")
	restored, err := vault.RestoreBackup(t.Context(), repository, docbank.BackupRestoreOptions{
		SnapshotID: result.snapshot.ID,
		Target:     target,
	})
	require.NoError(t, err)
	require.Equal(t, 1, restored.ExtrasFiles)
	got, err := os.ReadFile(filepath.Join(target, "host", "catalog.sqlite"))
	require.NoError(t, err)
	require.Equal(t, "host catalog\n", string(got))
}

func TestEmbeddedBackupRefusesSensitiveHostFileWithoutPlaintextOptIn(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(t.TempDir(), "live")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	createRangeFixture(t, vault, "/archive/photo.txt", []byte("snapshot content\n"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "backups"))
	require.NoError(t, err)

	secretPath := filepath.Join(t.TempDir(), "credentials.json")
	require.NoError(t, os.WriteFile(secretPath, []byte("synthetic secret\n"), 0o600))
	_, err = vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
		ExtraFiles: []docbank.BackupExtraFile{{
			Path: secretPath, RecordAs: "host/credentials.json", Sensitive: true,
		}},
	})
	require.ErrorContains(t, err, "requires an encrypted repository")
	snapshots, listErr := repository.Snapshots()
	require.NoError(t, listErr)
	require.Empty(t, snapshots)
}

func TestEmbeddedBackupPreparationFailureReleasesMutationGate(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(t.TempDir(), "live")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	createRangeFixture(t, vault, "/archive/photo.txt", []byte("snapshot content\n"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "backups"))
	require.NoError(t, err)

	prepareErr := errors.New("prepare host snapshot")
	_, err = vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
		Prepare: func(context.Context) error { return prepareErr },
	})
	require.ErrorIs(t, err, prepareErr)

	putDone := make(chan error, 1)
	go func() {
		_, putErr := vault.Put(t.Context(), "/archive/later.txt", bytes.NewBufferString("later\n"), docbank.PutOptions{
			MediaType: "text/plain",
		})
		putDone <- putErr
	}()
	select {
	case putErr := <-putDone:
		require.NoError(t, putErr)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "content mutation remained blocked after preparation failed")
	}
}

func TestEmbeddedBackupPreparationPanicReleasesMutationGate(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(t.TempDir(), "live")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	createRangeFixture(t, vault, "/archive/photo.txt", []byte("snapshot content\n"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "backups"))
	require.NoError(t, err)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
			Prepare: func(context.Context) error { panic("prepare host snapshot") },
		})
	}()
	require.Equal(t, "prepare host snapshot", recovered)

	putDone := make(chan error, 1)
	go func() {
		_, putErr := vault.Put(t.Context(), "/archive/later.txt", bytes.NewBufferString("later\n"), docbank.PutOptions{
			MediaType: "text/plain",
		})
		putDone <- putErr
	}()
	select {
	case putErr := <-putDone:
		require.NoError(t, putErr)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "content mutation remained blocked after preparation panicked")
	}
}

func TestEmbeddedBackupRoundTrip(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(t.TempDir(), "live")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	content := []byte("embedded backup content\n")
	created := createRangeFixture(t, vault, "/archive/photo.txt", content)

	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "backups"))
	require.NoError(t, err)
	snapshot, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{Tag: "before-edit"})
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.ID)
	require.Equal(t, "before-edit", snapshot.Tag)

	snapshots, err := repository.Snapshots()
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	require.Equal(t, snapshot.ID, snapshots[0].ID)

	verified, err := repository.Verify(t.Context(), docbank.BackupVerifyOptions{SnapshotID: snapshot.ID})
	require.NoError(t, err)
	require.Equal(t, []string{snapshot.ID}, verified.Snapshots)
	require.Empty(t, verified.Problems)

	target := filepath.Join(t.TempDir(), "restored")
	restored, err := vault.RestoreBackup(t.Context(), repository, docbank.BackupRestoreOptions{
		SnapshotID: snapshot.ID,
		Target:     target,
	})
	require.NoError(t, err)
	require.Equal(t, snapshot.ID, restored.SnapshotID)
	require.True(t, restored.Proof.ContentVerified)
	require.True(t, restored.Proof.SQLiteIntegrity)
	require.True(t, restored.Proof.ManifestStats)

	restoredVault, err := docbank.New(t.Context(), docbank.Config{Root: target})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredVault.Close()) })
	opened, err := restoredVault.OpenVersionContent(t.Context(), created.Version.ID)
	require.NoError(t, err)
	got, err := io.ReadAll(opened.Reader)
	require.NoError(t, err)
	require.NoError(t, opened.Reader.Verify())
	require.NoError(t, opened.Reader.Close())
	require.Equal(t, content, got)
}

func TestEmbeddedBackupFencesPhysicalMaintenance(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(t.TempDir(), "live")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	createRangeFixture(t, vault, "/archive/photo.txt", []byte("backup fence content\n"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "backups"))
	require.NoError(t, err)

	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	var resumeOnce sync.Once
	t.Cleanup(func() { resumeOnce.Do(func() { close(resume) }) })
	backupDone := make(chan error, 1)
	go func() {
		_, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
			Progress: func(progress docbank.BackupProgress) {
				if progress.Stage == "freeze" && progress.Final {
					pauseOnce.Do(func() { close(paused) })
					<-resume
				}
			},
		})
		backupDone <- err
	}()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "backup did not reach its post-freeze capture")
	}

	packDone := make(chan error, 1)
	go func() {
		_, err := vault.Pack(t.Context(), docbank.PackOptions{})
		packDone <- err
	}()
	select {
	case err := <-packDone:
		require.FailNow(t, "physical maintenance completed during backup capture", "error: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	resumeOnce.Do(func() { close(resume) })
	require.NoError(t, <-backupDone)
	require.NoError(t, <-packDone)
}

func TestEmbeddedBackupAllowsAppendsAfterFreeze(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(t.TempDir(), "live")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	createRangeFixture(t, vault, "/archive/photo.txt", []byte("snapshot content\n"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "backups"))
	require.NoError(t, err)

	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	var resumeOnce sync.Once
	t.Cleanup(func() { resumeOnce.Do(func() { close(resume) }) })
	backupDone := make(chan error, 1)
	go func() {
		_, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
			Progress: func(progress docbank.BackupProgress) {
				if progress.Stage == "freeze" && progress.Final {
					pauseOnce.Do(func() { close(paused) })
					<-resume
				}
			},
		})
		backupDone <- err
	}()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "backup did not reach its post-freeze capture")
	}

	putDone := make(chan error, 1)
	go func() {
		_, err := vault.Put(t.Context(), "/archive/later.txt", bytes.NewBufferString("later\n"), docbank.PutOptions{
			MediaType: "text/plain",
		})
		putDone <- err
	}()
	select {
	case err := <-putDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		resumeOnce.Do(func() { close(resume) })
		require.FailNow(t, "append remained blocked after the backup freeze ended")
	}

	resumeOnce.Do(func() { close(resume) })
	require.NoError(t, <-backupDone)
}

func TestEmbeddedRestoreRejectsLiveVaultOverlap(t *testing.T) {
	liveRoot := filepath.Join(t.TempDir(), "live")
	vault, err := docbank.New(t.Context(), docbank.Config{Root: liveRoot})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	createRangeFixture(t, vault, "/archive/photo.txt", []byte("overlap content\n"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "backups"))
	require.NoError(t, err)
	snapshot, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{})
	require.NoError(t, err)

	_, err = vault.RestoreBackup(t.Context(), repository, docbank.BackupRestoreOptions{
		SnapshotID: snapshot.ID,
		Target:     filepath.Join(liveRoot, "restore"),
	})
	require.ErrorIs(t, err, docbank.ErrBackupRestoreTargetOverlap)
}

func TestEmbeddedRestoreRejectsHostProtectedRoot(t *testing.T) {
	liveRoot := filepath.Join(t.TempDir(), "live")
	vault, err := docbank.New(t.Context(), docbank.Config{Root: liveRoot})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	createRangeFixture(t, vault, "/archive/photo.txt", []byte("protected content\n"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "backups"))
	require.NoError(t, err)
	snapshot, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{})
	require.NoError(t, err)

	protectedRoot := t.TempDir()
	t.Run("direct missing target", func(t *testing.T) {
		target := filepath.Join(protectedRoot, "missing-restore-target")
		_, restoreErr := vault.RestoreBackup(t.Context(), repository, docbank.BackupRestoreOptions{
			SnapshotID:     snapshot.ID,
			Target:         target,
			ProtectedRoots: []string{protectedRoot},
		})
		require.ErrorIs(t, restoreErr, docbank.ErrBackupRestoreTargetOverlap)
		require.NoDirExists(t, target)
	})

	t.Run("symlink alias with missing target", func(t *testing.T) {
		alias := filepath.Join(t.TempDir(), "protected-alias")
		if err := os.Symlink(protectedRoot, alias); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		target := filepath.Join(alias, "missing-restore-target")
		_, restoreErr := vault.RestoreBackup(t.Context(), repository, docbank.BackupRestoreOptions{
			SnapshotID:     snapshot.ID,
			Target:         target,
			ProtectedRoots: []string{protectedRoot},
		})
		require.ErrorIs(t, restoreErr, docbank.ErrBackupRestoreTargetOverlap)
		require.NoDirExists(t, filepath.Join(protectedRoot, "missing-restore-target"))
	})
}
