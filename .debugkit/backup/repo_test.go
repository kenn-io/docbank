package backup

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitCreatesLayout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	root := filepath.Join(t.TempDir(), "repo")
	r, err := Init(root)
	require.NoError(err)

	for _, dir := range []string{"snapshots", "packs", "indexes", "locks", "staging", "keys"} {
		info, statErr := os.Stat(filepath.Join(root, dir))
		require.NoError(statErr, dir)
		assert.True(info.IsDir(), dir)
	}
	cfg := r.Config()
	assert.Len(cfg.RepoID, 36)
	assert.Equal(FormatVersion, cfg.FormatVersion)
	assert.Equal(MinReaderVersion, cfg.MinReaderVersion)
	assert.Equal("none", cfg.Encryption)
	assert.NotEmpty(cfg.CreatedAt)
}

func TestInitRefusesExistingRepo(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	_, err := Init(root)
	require.NoError(t, err)
	_, err = Init(root)
	require.Error(t, err)
}

func TestOpenRoundTrip(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	created, err := Init(root)
	require.NoError(err)

	opened, err := Open(root)
	require.NoError(err)
	assert.Equal(t, created.Config().RepoID, opened.Config().RepoID)
}

func TestOpenRejectsFutureRepo(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	_, err := Init(root)
	require.NoError(err)

	data, err := os.ReadFile(filepath.Join(root, "config.toml"))
	require.NoError(err)

	munged := replaceInTOML(string(data), "min_reader_version", "99")
	require.NoError(os.WriteFile(filepath.Join(root, "config.toml"), []byte(
		munged), 0o600))

	_, err = Open(root)
	require.ErrorContains(err, "upgrade the reader")
}

func TestRequireReaderVersion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	repo, err := Init(root)
	require.NoError(err)

	require.ErrorContains(
		repo.RequireReaderVersion(SupportedReaderVersion+1),
		"cannot require reader version")

	require.NoError(repo.RequireReaderVersion(SupportedReaderVersion))
	require.NoError(repo.RequireReaderVersion(3))
	assert.Equal(SupportedReaderVersion, repo.Config().MinReaderVersion)
	opened, err := Open(root)
	require.NoError(err)
	assert.Equal(SupportedReaderVersion, opened.Config().MinReaderVersion)
}

func TestConfigUpdatesRefuseRepoThisReaderCannotOpen(t *testing.T) {
	require := require.New(t)
	repo := initTestRepo(t)
	path := repo.Path(repoConfigName)
	data, err := os.ReadFile(path)
	require.NoError(err)
	require.NoError(os.WriteFile(path, []byte(replaceInTOML(string(data),
		"min_reader_version", "99")), 0o600))

	// An unchanged page size still checks the repository on disk.
	require.ErrorContains(repo.SetPageSize(repo.Config().PageSize),
		"upgrade the reader")
	require.ErrorContains(repo.RequireReaderVersion(SupportedReaderVersion),
		"upgrade the reader")

	data, err = os.ReadFile(path)
	require.NoError(err)
	assert.Contains(t, string(data), "min_reader_version = 99")
}

func TestConfigUpdatesRefuseReplacedRepo(t *testing.T) {
	require := require.New(t)
	repo := initTestRepo(t)
	original := repo.Config()
	require.NoError(os.RemoveAll(repo.Root()))
	replacement, err := Init(repo.Root())
	require.NoError(err)

	require.ErrorContains(repo.SetPageSize(4096), "reopen it")
	assert.Equal(t, original, repo.Config())
	reopened, err := Open(repo.Root())
	require.NoError(err)
	assert.Equal(t, replacement.Config(), reopened.Config())
}

func TestConfigUpdatesKeepOtherHandleChanges(t *testing.T) {
	// newHandles returns two handles opened before either changes anything,
	// so the second is stale once the first writes.
	newHandles := func(t *testing.T) (root string, stale, other *Repo) {
		t.Helper()
		other = initTestRepo(t)
		root = other.Root()
		stale, err := Open(root)
		require.NoError(t, err)
		return root, stale, other
	}

	t.Run("SetPageSize keeps reader version", func(t *testing.T) {
		root, stale, other := newHandles(t)

		require.NoError(t, other.RequireReaderVersion(7))
		require.NoError(t, stale.SetPageSize(4096))

		reopened, err := Open(root)
		require.NoError(t, err)
		require.Equal(t, 7, reopened.Config().MinReaderVersion)
		assert.Equal(t, 4096, reopened.Config().PageSize)
	})

	t.Run("stale handle refreshes to the requirement another handle wrote", func(t *testing.T) {
		_, stale, other := newHandles(t)

		require.NoError(t, other.RequireReaderVersion(7))
		require.NoError(t, stale.RequireReaderVersion(7))

		assert.Equal(t, 7, stale.Config().MinReaderVersion)
	})
}

func TestRequireReaderVersionFailedPublishKeepsHandleOnDiskConfig(t *testing.T) {
	require := require.New(t)
	repo := initTestRepo(t)

	// Publication stages under staging/; without it the write fails.
	require.NoError(os.RemoveAll(repo.Path(stagingDirName)))

	require.ErrorContains(repo.RequireReaderVersion(7), "publishing config.toml")
	assert.Equal(t, MinReaderVersion, repo.Config().MinReaderVersion)

	reopened, err := Open(repo.Root())
	require.NoError(err)
	assert.Equal(t, MinReaderVersion, reopened.Config().MinReaderVersion)
}

func replaceInTOML(content, key, value string) string {
	re := regexp.MustCompile(key + ` = \d+`)
	return re.ReplaceAllString(content, key+` = `+value)
}

// TestOpenRejectsNonCanonicalRepoID pins that Open refuses a config.toml
// whose repo_id is not the canonical generated UUID: the ID is joined into
// cache filenames, so a tampered value carrying path separators could
// otherwise address files outside the cache directory.
func TestOpenRejectsNonCanonicalRepoID(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	_, err := Init(root)
	require.NoError(err)

	cfgPath := filepath.Join(root, "config.toml")
	data, err := os.ReadFile(cfgPath)
	require.NoError(err)
	re := regexp.MustCompile(`repo_id = "[^"]*"`)

	for _, id := range []string{
		`../../../home/user/.ssh/authorized_keys`,
		`sub/dir`, ``, `AAAAAAAA-0000-4000-8000-000000000000`,
	} {
		munged := re.ReplaceAllString(string(data), `repo_id = "`+id+`"`)
		require.NoError(os.WriteFile(cfgPath, []byte(munged), 0o600))
		_, err = Open(root)
		require.ErrorContains(err, "not the canonical UUID", "repo_id %q", id)
	}
}

func TestCleanStaging(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	r, err := Init(root)
	require.NoError(err)

	debris := filepath.Join(root, "staging", "leftover.tmp")
	require.NoError(os.WriteFile(debris, []byte("x"), 0o600))
	require.NoError(r.CleanStaging())
	_, statErr := os.Stat(debris)
	require.True(os.IsNotExist(statErr))
}

func TestWriteFileAtomic(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	r, err := Init(root)
	require.NoError(err)

	require.NoError(writeFileAtomic(r, filepath.Join("indexes", "x.mvidx"), []byte("hello")))
	got, err := os.ReadFile(r.Path("indexes", "x.mvidx"))
	require.NoError(err)
	assert.Equal(t, []byte("hello"), got)
	entries, err := os.ReadDir(r.Path("staging"))
	require.NoError(err)
	assert.Empty(t, entries)
}

// TestCleanStagingRefusesSymlinkedStaging pins that staging cleanup never
// follows a planted symlink: if another principal replaces the staging
// directory with a link, cleanup must refuse rather than delete entries in
// the link's target as the calling user.
func TestCleanStagingRefusesSymlinkedStaging(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "precious.txt")
	require.NoError(os.WriteFile(victim, []byte("do not delete"), 0o600))

	staging := r.Path("staging")
	require.NoError(os.RemoveAll(staging))
	if err := os.Symlink(victimDir, staging); err != nil {
		// Windows restricts symlink creation to elevated or developer-mode
		// users; the guard under test is platform-independent.
		t.Skip("symlinks not supported on this platform")
	}

	err := r.CleanStaging()
	require.ErrorContains(err, "not a directory")
	_, statErr := os.Stat(victim)
	require.NoError(statErr, "the symlink target's contents must be untouched")
}
