//go:build unix

package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestRestoreBeforePublicationRejectsInvalidPrivateOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.Remove(dbPath))
				outside := filepath.Join(t.TempDir(), "outside.db")
				require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
				require.NoError(t, os.Symlink(outside, dbPath))
			},
		},
		{
			name: "directory",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.Remove(dbPath))
				require.NoError(t, os.Mkdir(dbPath, 0o700))
			},
		},
		{
			name: "wal sidecar",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(dbPath+"-wal", []byte("wal"), 0o600))
			},
		},
		{
			name: "shm sidecar",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(dbPath+"-shm", []byte("shm"), 0o600))
			},
		},
		{
			name: "journal sidecar",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(dbPath+"-journal", []byte("journal"), 0o600))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ctx := t.Context()
			r := initTestRepo(t)
			dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
			_, err := Create(ctx, r, newTestApp(), createOpts(
				dbPath, attachmentsDir, dataDir, t.TempDir(),
			))
			require.NoError(err)

			target := filepath.Join(t.TempDir(), "restore")
			original := []byte("existing database remains unpublished")
			require.NoError(os.Mkdir(target, 0o700))
			require.NoError(os.WriteFile(filepath.Join(target, newTestApp().DBFileName()), original, 0o600))
			beforeScratch := restorePublicationScratchDirs(t, r)
			_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
				TargetDir: target,
				Overwrite: true,
				BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
					tc.mutate(t, staged.DBPath)
					return nil
				},
			})
			require.Error(err)
			got, readErr := os.ReadFile(filepath.Join(target, newTestApp().DBFileName()))
			require.NoError(readErr)
			assert.Equal(original, got, "invalid callback output must fail before canonical publication")
			assert.Empty(restoreDatabaseStageFiles(t, target, newTestApp().DBFileName()))
			assert.Equal(beforeScratch, restorePublicationScratchDirs(t, r))
		})
	}
}

func TestRestoreBeforePublicationCannotWriteReplacedTargetNamespace(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(
		dbPath, attachmentsDir, dataDir, t.TempDir(),
	))
	require.NoError(err)

	parent := t.TempDir()
	target := filepath.Join(parent, "restore")
	heldTarget := filepath.Join(parent, "restore-held")
	var attackerDB string
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target,
		BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
			require.NoError(os.Rename(target, heldTarget))
			require.NoError(os.Mkdir(target, 0o700))
			attackerDB = filepath.Join(target, filepath.Base(staged.DBPath))
			attacker, err := sql.Open("sqlite3", attackerDB)
			require.NoError(err)
			require.NoError(func() error {
				_, err := attacker.ExecContext(t.Context(), "PRAGMA user_version = 99")
				return err
			}())
			require.NoError(attacker.Close())

			callbackDB, err := sql.Open("sqlite3", staged.DBPath)
			require.NoError(err)
			require.NoError(func() error {
				_, err := callbackDB.ExecContext(t.Context(), "PRAGMA user_version = 1")
				return err
			}())
			require.NoError(callbackDB.Close())
			return nil
		},
	})
	require.Error(err, "the replaced target must never be accepted for later proof or publication")

	attacker, err := sql.Open("sqlite3", attackerDB)
	require.NoError(err)
	defer func() { _ = attacker.Close() }()
	var userVersion int
	require.NoError(attacker.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&userVersion))
	require.Equal(99, userVersion, "callback writes must not reach the attacker-selected database")
}

func TestRestoreBeforePublicationResolvedStagingCannotBeRedirected(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(
		dbPath, attachmentsDir, dataDir, t.TempDir(),
	))
	require.NoError(err)

	externalStaging := t.TempDir()
	require.NoError(os.Remove(r.Path(stagingDirName)))
	require.NoError(os.Symlink(externalStaging, r.Path(stagingDirName)))
	attackerStore := t.TempDir()
	target := r.Root()
	heldTarget := target + "-held"
	var attackerDB string
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target,
		Overwrite: true,
		BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
			privateDirName := filepath.Base(filepath.Dir(staged.DBPath))
			require.NoError(os.Rename(target, heldTarget))
			require.NoError(os.Mkdir(target, 0o700))
			attackerDir := filepath.Join(target, stagingDirName, privateDirName)
			require.NoError(os.MkdirAll(filepath.Dir(attackerDir), 0o700))
			require.NoError(os.Symlink(attackerStore, attackerDir))
			attackerDB = filepath.Join(attackerStore, filepath.Base(staged.DBPath))
			attacker, err := sql.Open("sqlite3", attackerDB)
			require.NoError(err)
			require.NoError(func() error {
				_, err := attacker.ExecContext(t.Context(), "PRAGMA user_version = 99")
				return err
			}())
			require.NoError(attacker.Close())

			callbackDB, err := sql.Open("sqlite3", staged.DBPath)
			require.NoError(err)
			require.NoError(func() error {
				_, err := callbackDB.ExecContext(t.Context(), "PRAGMA user_version = 1")
				return err
			}())
			require.NoError(callbackDB.Close())
			return nil
		},
	})
	require.Error(err, "the replaced target must never be accepted for later proof or publication")

	attacker, err := sql.Open("sqlite3", attackerDB)
	require.NoError(err)
	defer func() { _ = attacker.Close() }()
	var userVersion int
	require.NoError(attacker.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&userVersion))
	require.Equal(99, userVersion, "callback writes must not reach the symlinked-staging namespace")
}

func TestRestoreBeforePublicationRelativeTargetCannotReachNestedRepositoryStaging(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	base := t.TempDir()
	t.Chdir(base)
	target := "target"
	targetPath := filepath.Join(base, target)
	r, err := Init(filepath.Join(targetPath, "repo"))
	require.NoError(err)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err = Create(ctx, r, newTestApp(), createOpts(
		dbPath, attachmentsDir, dataDir, t.TempDir(),
	))
	require.NoError(err)

	heldTarget := targetPath + "-held"
	attackerStore := t.TempDir()
	var attackerDB string
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target,
		Overwrite: true,
		BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
			privateDirName := filepath.Base(filepath.Dir(staged.DBPath))
			require.NoError(os.Rename(targetPath, heldTarget))
			require.NoError(os.Mkdir(targetPath, 0o700))
			attackerDir := filepath.Join(targetPath, "repo", stagingDirName, privateDirName)
			require.NoError(os.MkdirAll(filepath.Dir(attackerDir), 0o700))
			require.NoError(os.Symlink(attackerStore, attackerDir))
			attackerDB = filepath.Join(attackerStore, filepath.Base(staged.DBPath))
			attacker, err := sql.Open("sqlite3", attackerDB)
			require.NoError(err)
			require.NoError(func() error {
				_, err := attacker.ExecContext(t.Context(), "PRAGMA user_version = 99")
				return err
			}())
			require.NoError(attacker.Close())

			callbackDB, err := sql.Open("sqlite3", staged.DBPath)
			require.NoError(err)
			require.NoError(func() error {
				_, err := callbackDB.ExecContext(t.Context(), "PRAGMA user_version = 1")
				return err
			}())
			require.NoError(callbackDB.Close())
			return nil
		},
	})
	require.Error(err, "the replaced target must never be accepted for later proof or publication")

	attacker, err := sql.Open("sqlite3", attackerDB)
	require.NoError(err)
	defer func() { _ = attacker.Close() }()
	var userVersion int
	require.NoError(attacker.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&userVersion))
	require.Equal(99, userVersion, "callback writes must not reach nested repository staging through a relative target")
}

func TestPrepareBeforePublicationRemovesStagedReplacementAfterScratchCleanupFailure(t *testing.T) {
	require := require.New(t)
	target := t.TempDir()
	root, err := openRestoreRoot(target)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(root.Close()) })
	currentRel := "app.db.restore-" + pack.NewPackID()
	require.NoError(os.WriteFile(filepath.Join(target, currentRel), []byte("database"), 0o600))
	st := &restoreState{root: root, target: target, repo: initTestRepo(t)}
	var blockedDir string

	_, _, err = st.prepareBeforePublication(
		t.Context(), currentRel, "app.db",
		func(_ context.Context, staged RestorePublicationTarget) error {
			blockedDir = filepath.Join(filepath.Dir(staged.DBPath), "blocked")
			if err := os.Mkdir(blockedDir, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(blockedDir, "child"), []byte("block cleanup"), 0o600); err != nil {
				return err
			}
			return os.Chmod(blockedDir, 0)
		},
	)

	require.ErrorContains(err, "removing private publication staging directory")
	assert.Empty(t, restoreDatabaseStageFiles(t, target, "app.db"))
	require.NoError(os.Chmod(blockedDir, 0o700))
	privateDir := filepath.Dir(blockedDir)
	scratchRoot := filepath.Dir(privateDir)
	require.NoError(os.RemoveAll(privateDir))
	require.NoError(os.Remove(scratchRoot))
}
