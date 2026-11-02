package humacheck

import (
	"io"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gittest "go.kenn.io/kit/git/test"
)

func TestIndexFSReadsStagedContent(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	repo := gittest.NewRepo(t, gittest.Options{ResolvePath: true})
	repo.WriteFile("api/openapi.yaml", "openapi: 3.1.0\n")
	repo.WriteFile("go.mod", "module m\n")
	repo.Run("add", "-A")
	// An unstaged edit must not be what the checker sees.
	repo.WriteFile("api/openapi.yaml", "openapi: 2.0\n")

	ctx := t.Context()
	root, inGit, err := gitTopLevel(ctx, repo.Root)
	require.NoError(err)
	require.True(inGit)
	assert.Equal(repo.Root, root)

	tracked, err := trackedFiles(ctx, root)
	require.NoError(err)
	assert.Equal([]string{"api/openapi.yaml", "go.mod"}, tracked)

	changed, err := unstagedChanges(ctx, root)
	require.NoError(err)
	assert.Equal([]string{"api/openapi.yaml"}, changed)

	index, err := newIndexFS(ctx, root)
	require.NoError(err)
	t.Cleanup(func() { _ = index.Close() })

	content, err := fs.ReadFile(index, "api/openapi.yaml")
	require.NoError(err)
	assert.Equal("openapi: 3.1.0\n", string(content))

	f, err := index.Open("go.mod")
	require.NoError(err)
	info, err := f.Stat()
	require.NoError(err)
	assert.Equal(int64(len("module m\n")), info.Size())
	data, err := io.ReadAll(f)
	require.NoError(err)
	assert.Equal("module m\n", string(data))
	require.NoError(f.Close())

	_, err = index.Open("missing.txt")
	require.ErrorIs(err, fs.ErrNotExist)
	_, err = index.Open("../escape")
	assert.ErrorIs(err, fs.ErrInvalid)
}

func TestGitTopLevelOutsideRepository(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	_, inGit, err := gitTopLevel(t.Context(), dir)
	require.NoError(t, err)
	assert.False(t, inGit)
}
