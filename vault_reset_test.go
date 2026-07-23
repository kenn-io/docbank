package docbank

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/home"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

func TestResetVaultRecoversCorruptCatalogWithoutDeletingDiagnosticVault(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "vault")
	diagnostic := filepath.Join(base, "vault.reset-20260722T120000Z")
	vault, err := New(t.Context(), Config{Root: root, SQLite: modernc.Driver{}})
	require.NoError(t, err)
	_, err = vault.Put(t.Context(), "/kept.txt", bytes.NewBufferString("kept bytes\n"), PutOptions{})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	corrupt := []byte("not a sqlite catalog")
	require.NoError(t, os.WriteFile(filepath.Join(root, "docbank.db"), corrupt, 0o600))

	fresh, err := ResetVault(t.Context(), Config{Root: root, SQLite: modernc.Driver{}},
		ResetOptions{DiagnosticRoot: diagnostic})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })

	assert.FileExists(t, filepath.Join(root, "docbank.db"))
	assert.DirExists(t, diagnostic)
	assert.Equal(t, corrupt, mustReadResetFile(t, filepath.Join(diagnostic, "docbank.db")))
	digest := contentIdentity([]byte("kept bytes\n")).SHA256
	assert.Equal(t, []byte("kept bytes\n"),
		mustReadResetFile(t, filepath.Join(diagnostic, "blobs", digest[:2], digest)))
	_, err = fresh.Stat(t.Context(), "/kept.txt")
	require.ErrorIs(t, err, ErrNotFound, "the replacement must be a fresh logical vault")
	contender, err := New(t.Context(), Config{Root: root, SQLite: modernc.Driver{}})
	if contender != nil {
		_ = contender.Close()
	}
	require.Error(t, err, "the returned fresh vault must retain ordinary ownership")
}

func TestResetVaultValidatesSourceAndDestinationWithoutMutation(t *testing.T) {
	t.Run("invalid config before release callback", func(t *testing.T) {
		called := false
		fresh, err := ResetVault(t.Context(), Config{
			Root: filepath.Join(t.TempDir(), "vault"),
			LooseCompression: LooseCompressionOptions{
				MinSavingsPercent: 101,
			},
		}, ResetOptions{
			DiagnosticRoot: filepath.Join(t.TempDir(), "vault.reset"),
			ReleaseCurrent: func() error { called = true; return nil },
		})

		assert.Nil(t, fresh)
		require.Error(t, err)
		assert.False(t, called)
	})

	t.Run("missing source", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "missing")
		diagnostic := filepath.Join(base, "missing.reset")

		fresh, err := ResetVault(t.Context(), Config{Root: root, SQLite: modernc.Driver{}},
			ResetOptions{DiagnosticRoot: diagnostic})

		assert.Nil(t, fresh)
		require.Error(t, err)
		assert.NoDirExists(t, root)
		assert.NoDirExists(t, diagnostic)
	})

	t.Run("non sibling destination", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "vault")
		vault, err := New(t.Context(), Config{Root: root, SQLite: modernc.Driver{}})
		require.NoError(t, err)
		require.NoError(t, vault.Close())
		called := false

		fresh, err := ResetVault(t.Context(), Config{Root: root, SQLite: modernc.Driver{}},
			ResetOptions{
				DiagnosticRoot: filepath.Join(t.TempDir(), "vault.reset"),
				ReleaseCurrent: func() error { called = true; return nil },
			})

		assert.Nil(t, fresh)
		require.Error(t, err)
		assert.False(t, called)
		assert.DirExists(t, root)
	})

	t.Run("symlink source", func(t *testing.T) {
		for name, suffix := range map[string]string{
			"direct":             "",
			"trailing separator": string(os.PathSeparator),
			"trailing dot":       string(os.PathSeparator) + ".",
		} {
			t.Run(name, func(t *testing.T) {
				base := t.TempDir()
				realRoot := filepath.Join(base, "real-vault")
				vault, err := New(
					t.Context(), Config{Root: realRoot, SQLite: modernc.Driver{}})
				require.NoError(t, err)
				require.NoError(t, vault.Close())
				alias := filepath.Join(base, "vault-alias")
				if err := os.Symlink(realRoot, alias); err != nil {
					t.Skipf("creating a symlink requires additional platform permission: %v", err)
				}

				fresh, err := ResetVault(
					t.Context(), Config{Root: alias + suffix, SQLite: modernc.Driver{}},
					ResetOptions{DiagnosticRoot: filepath.Join(base, "vault.reset")})
				if fresh != nil {
					t.Cleanup(func() { _ = fresh.Close() })
				}

				assert.Nil(t, fresh)
				require.Error(t, err)
				require.ErrorContains(t, err, "must not be a symlink")
				assert.DirExists(t, realRoot)
				info, statErr := os.Lstat(alias)
				require.NoError(t, statErr)
				assert.NotZero(t, info.Mode()&os.ModeSymlink)
				assert.NoDirExists(t, filepath.Join(base, "vault.reset"))
			})
		}
	})

	t.Run("existing diagnostic destination", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "vault")
		diagnostic := filepath.Join(base, "vault.reset")
		vault, err := New(t.Context(), Config{Root: root, SQLite: modernc.Driver{}})
		require.NoError(t, err)
		require.NoError(t, vault.Close())
		sourceBefore := mustReadResetFile(t, filepath.Join(root, "docbank.db"))
		require.NoError(t, os.Mkdir(diagnostic, 0o700))
		sentinel := filepath.Join(diagnostic, "sentinel")
		require.NoError(t, os.WriteFile(sentinel, []byte("do not replace"), 0o600))

		fresh, err := ResetVault(t.Context(), Config{Root: root, SQLite: modernc.Driver{}},
			ResetOptions{DiagnosticRoot: diagnostic})

		assert.Nil(t, fresh)
		require.Error(t, err)
		assert.Equal(t, sourceBefore, mustReadResetFile(t, filepath.Join(root, "docbank.db")))
		assert.Equal(t, []byte("do not replace"), mustReadResetFile(t, sentinel))
	})
}

func TestResetVaultCoordinatesReleaseOfCurrentOwnerBeforeRename(t *testing.T) {
	grandparent := t.TempDir()
	base := filepath.Join(grandparent, "base")
	require.NoError(t, os.Mkdir(base, 0o700))
	root := filepath.Join(base, "vault")
	diagnostic := filepath.Join(base, "vault.reset")
	current, err := New(t.Context(), Config{Root: root, SQLite: modernc.Driver{}})
	require.NoError(t, err)
	released := make(chan struct{})
	continueReset := make(chan struct{})
	result := make(chan struct {
		vault *Vault
		err   error
	}, 1)
	go func() {
		fresh, resetErr := ResetVault(t.Context(), Config{Root: root, SQLite: modernc.Driver{}},
			ResetOptions{
				DiagnosticRoot: diagnostic,
				ReleaseCurrent: func() error {
					err := current.Close()
					close(released)
					<-continueReset
					return err
				},
			})
		result <- struct {
			vault *Vault
			err   error
		}{fresh, resetErr}
	}()
	<-released

	contenderErrors := make(map[string]error)
	for name, contenderRoot := range map[string]string{
		"exact":       root,
		"parent":      base,
		"grandparent": grandparent,
		"child":       filepath.Join(root, "child"),
	} {
		contender, contenderErr := New(
			t.Context(), Config{Root: contenderRoot, SQLite: modernc.Driver{}})
		if contender != nil {
			_ = contender.Close()
		}
		contenderErrors[name] = contenderErr
	}
	assert.DirExists(t, root, "reset must not rename until ReleaseCurrent returns")
	assert.NoDirExists(t, diagnostic)

	close(continueReset)
	reset := <-result
	require.NoError(t, reset.err)
	require.NotNil(t, reset.vault)
	require.NoError(t, reset.vault.Close())
	assert.DirExists(t, root)
	assert.DirExists(t, diagnostic)
	for name, contenderErr := range contenderErrors {
		require.ErrorIs(t, contenderErr, home.ErrVaultLocked,
			"reset must exclude the %s vault root after current ownership is released", name)
	}
}

func TestNewAllowsCaseDistinctVaultRoots(t *testing.T) {
	base := t.TempDir()
	upperRoot := filepath.Join(base, "CaseDistinctVault")
	lowerRoot := filepath.Join(base, "casedistinctvault")
	require.NoError(t, os.Mkdir(upperRoot, 0o700))
	if err := os.Mkdir(lowerRoot, 0o700); errors.Is(err, os.ErrExist) {
		t.Skip("filesystem is case-insensitive")
	} else {
		require.NoError(t, err)
	}
	upperInfo, err := os.Stat(upperRoot)
	require.NoError(t, err)
	lowerInfo, err := os.Stat(lowerRoot)
	require.NoError(t, err)
	if os.SameFile(upperInfo, lowerInfo) {
		t.Skip("filesystem does not provide case-distinct directory identities")
	}

	upper, err := New(t.Context(), Config{Root: upperRoot, SQLite: modernc.Driver{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upper.Close()) })
	lower, err := New(t.Context(), Config{Root: lowerRoot, SQLite: modernc.Driver{}})
	require.NoError(t, err,
		"case-distinct vaults on a case-sensitive filesystem must not share ownership")
	require.NoError(t, lower.Close())
}

func TestResetVaultLockConflictLeavesActiveSourceUnmoved(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "vault")
	diagnostic := filepath.Join(base, "vault.reset")
	current, err := New(t.Context(), Config{Root: root, SQLite: modernc.Driver{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, current.Close()) })

	fresh, err := ResetVault(t.Context(), Config{Root: root, SQLite: modernc.Driver{}},
		ResetOptions{DiagnosticRoot: diagnostic})

	assert.Nil(t, fresh)
	require.Error(t, err)
	assert.DirExists(t, root)
	assert.NoDirExists(t, diagnostic)
}

func TestResetVaultReleaseFailureLeavesSourceUnmoved(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "vault")
	diagnostic := filepath.Join(base, "vault.reset")
	vault, err := New(t.Context(), Config{Root: root, SQLite: modernc.Driver{}})
	require.NoError(t, err)
	releaseErr := errors.New("close current owner failed")

	fresh, err := ResetVault(t.Context(), Config{Root: root, SQLite: modernc.Driver{}},
		ResetOptions{DiagnosticRoot: diagnostic, ReleaseCurrent: func() error { return releaseErr }})

	assert.Nil(t, fresh)
	require.ErrorIs(t, err, releaseErr)
	assert.DirExists(t, root)
	assert.NoDirExists(t, diagnostic)
	require.NoError(t, vault.Close())
}

func TestResetVaultFreshInitializationFailurePreservesBothPaths(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "vault")
	diagnostic := filepath.Join(base, "vault.reset")
	vault, err := New(t.Context(), Config{Root: root, SQLite: modernc.Driver{}})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	canonicalRoot, err := home.CanonicalRoot(root)
	require.NoError(t, err)
	canonicalDiagnostic, err := home.CanonicalRoot(diagnostic)
	require.NoError(t, err)
	sourceBefore := mustReadResetFile(t, filepath.Join(root, "docbank.db"))
	openErr := errors.New("fresh sqlite open failed")

	fresh, err := ResetVault(t.Context(), Config{Root: root, SQLite: failingResetDriver{err: openErr}},
		ResetOptions{DiagnosticRoot: diagnostic})

	assert.Nil(t, fresh)
	require.ErrorIs(t, err, openErr)
	require.ErrorContains(t, err, canonicalRoot)
	require.ErrorContains(t, err, canonicalDiagnostic)
	assert.DirExists(t, root, "failed fresh initialization must not delete its partial vault")
	assert.DirExists(t, diagnostic)
	assert.Equal(t, sourceBefore, mustReadResetFile(t, filepath.Join(diagnostic, "docbank.db")))
}

type failingResetDriver struct{ err error }

func (f failingResetDriver) Name() string { return "failing reset test driver" }
func (f failingResetDriver) Open(string, docsqlite.OpenOptions) (*sql.DB, error) {
	return nil, f.err
}
func (f failingResetDriver) IsBusy(error) bool            { return false }
func (f failingResetDriver) IsUniqueViolation(error) bool { return false }

func mustReadResetFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}

var _ docsqlite.Driver = failingResetDriver{}
