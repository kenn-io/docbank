package docbank_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank/docbank"
	"go.kenn.io/kit/backup"
)

func TestEmbeddedBackupCapturesHostFilePreparedInsideFreeze(t *testing.T) {
	t.Parallel()
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
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: the timer shows the event does not happen; the paused host preparation keeps the put blocked
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestEmbeddedBackup(t *testing.T) {
	t.Parallel()
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

	t.Run("verify-error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := repository.Verify(ctx, docbank.BackupVerifyOptions{SnapshotID: snapshot.ID})
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, strings.Count(err.Error(), "verifying backup repository"))
	})

	t.Run("round-trip", func(t *testing.T) {
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
	})
	t.Run("unsupported-coverage", func(t *testing.T) {
		physical, err := backup.Open(repository.Root())
		require.NoError(t, err)
		manifest, err := physical.LoadManifest(snapshot.ID)
		require.NoError(t, err)
		var stats map[string]any
		require.NoError(t, json.Unmarshal(manifest.Stats, &stats))
		stats["coverage"] = map[string]any{"version": 2, "included": []string{"future-shape"}}
		manifest.Stats, err = json.Marshal(stats)
		require.NoError(t, err)
		futureID, err := physical.WriteManifest(manifest)
		require.NoError(t, err)
		snapshots, err := repository.Snapshots()
		require.NoError(t, err)
		require.Len(t, snapshots, 2)
		originalIdx := slices.IndexFunc(snapshots, func(s docbank.BackupSnapshot) bool { return s.ID == snapshot.ID })
		require.GreaterOrEqual(t, originalIdx, 0)
		require.NotNil(t, snapshots[originalIdx].Coverage)
		require.Equal(t, snapshot.Coverage, snapshots[originalIdx].Coverage)
		require.Empty(t, snapshots[originalIdx].CoverageWarning)
		idx := slices.IndexFunc(snapshots, func(s docbank.BackupSnapshot) bool { return s.ID == futureID })
		require.GreaterOrEqual(t, idx, 0)
		require.Equal(t, snapshot.BlobBytes, snapshots[idx].BlobBytes)
		require.Nil(t, snapshots[idx].Coverage)
		require.Contains(t, snapshots[idx].CoverageWarning, "version 2")
		verified, err := repository.Verify(t.Context(), docbank.BackupVerifyOptions{SnapshotID: futureID, Quick: true})
		require.NoError(t, err)
		require.Equal(t, []string{futureID}, verified.Snapshots)
		require.Equal(t, []docbank.BackupVerifyProblem{{SnapshotID: futureID, Detail: "backupapp: unsupported backup coverage version 2; upgrade Docbank to verify this snapshot"}}, verified.Problems)

		target := filepath.Join(t.TempDir(), "future")
		_, err = vault.RestoreBackup(t.Context(), repository, docbank.BackupRestoreOptions{SnapshotID: futureID, Target: target})
		unsupported, ok := errors.AsType[*docbank.UnsupportedBackupCoverageError](err)
		require.True(t, ok, "restore error %v", err)
		require.Equal(t, 2, unsupported.Version)
		require.ErrorContains(t, err, "upgrade Docbank to restore this snapshot")
		require.NoFileExists(t, filepath.Join(target, "docbank.db"))
	})
}

func TestEmbeddedBackupFencesPhysicalMaintenance(t *testing.T) {
	t.Parallel()
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
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: the timer shows the event does not happen; the paused backup capture keeps the pack blocked
	}

	resumeOnce.Do(func() { close(resume) })
	require.NoError(t, <-backupDone)
	require.NoError(t, <-packDone)
}

func TestEmbeddedBackupAllowsAppendsAfterFreeze(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
