package gitrepo

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
)

func TestIsUnbornHead(t *testing.T) {
	repo := gittest.NewRepo(t, gittest.Options{ConfigureUser: true})
	if !IsUnbornHead(t.Context(), repo.Root) {
		require.FailNow(t, "newly initialized repo should have unborn HEAD")
	}

	repo.CommitFile("a.txt", "a\n", "initial")
	if IsUnbornHead(t.Context(), repo.Root) {
		require.FailNow(t, "repo with a commit should not have unborn HEAD")
	}
}

func TestIsAncestorDistinguishesNegativeFromGitErrors(t *testing.T) {
	repo := gittest.NewRepoWithCommit(t)
	base := repo.Head()
	repo.CommitFile("b.txt", "b\n", "second")
	head := repo.Head()

	ok, err := IsAncestor(t.Context(), repo.Root, base, head)
	if err != nil || !ok {
		require.FailNow(t, fmt.Sprintf("base should be ancestor of head: ok=%v err=%v", ok, err))
	}

	ok, err = IsAncestor(t.Context(), repo.Root, head, base)
	if err != nil {
		require.FailNow(t, fmt.Sprintf("non-ancestor should not be an error: %v", err))
	}
	if ok {
		require.FailNow(t, "head should not be ancestor of base")
	}
}

func TestWorktreePathForBranchSkipsStalePaths(t *testing.T) {
	require := require.New(t)
	repo := gittest.NewRepoWithCommit(t)
	wt := repo.AddWorktree("feature")

	path, ok, err := WorktreePathForBranch(t.Context(), repo.Root, "feature")
	if err != nil || !ok || path != wt {
		require.FailNow(fmt.Sprintf("path=%q ok=%v err=%v, want %q true nil", path, ok, err, wt))
	}

	if err := os.RemoveAll(wt); err != nil {
		require.FailNow(err.Error())
	}
	path, ok, err = WorktreePathForBranch(t.Context(), repo.Root, "feature")
	if err != nil {
		require.FailNow(err.Error())
	}
	if ok || path != repo.Root {
		require.FailNow(fmt.Sprintf("stale worktree should fall back to repo root false, got %q %v", path, ok))
	}
}

func TestMainRootReturnsCheckoutRootForBareBackedWorktree(t *testing.T) {
	ctx := t.Context()
	bareRepo := gittest.NewBareRepo(t)
	seedRepo := gittest.NewRepoWithCommit(t)
	seedRepo.Run("remote", "add", "origin", bareRepo.Root)
	seedRepo.Run("push", "origin", "HEAD:main")

	worktreeDir := filepath.Join(t.TempDir(), "worktree")
	bareRepo.Run("worktree", "add", worktreeDir, "main")
	t.Cleanup(func() {
		_, _, _ = bareRepo.Runner.Run(ctx, bareRepo.Root, nil, "worktree", "remove", "--force", worktreeDir)
	})

	got, err := MainRoot(ctx, worktreeDir)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	want, err := Root(ctx, worktreeDir)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	if got != want {
		require.FailNow(t, fmt.Sprintf("MainRoot() = %q, want checkout root %q", got, want))
	}
}

func TestLooksLikeSHAUsesLengthAndHexPattern(t *testing.T) {
	for _, value := range []string{"abcdef0", "ABCDEF0123456789"} {
		if !LooksLikeSHA(value) {
			require.FailNow(t, fmt.Sprintf("%q should look like a SHA", value))
		}
	}
	for _, value := range []string{"dead", "not-a-sha", "abcdefg"} {
		if LooksLikeSHA(value) {
			require.FailNow(t, fmt.Sprintf("%q should not look like a SHA", value))
		}
	}
}

func TestHooksPathAndEnsureAbsoluteHooksPath(t *testing.T) {
	gitDirHooks := filepath.Join(".git", "custom-hooks")
	outsideHooks := filepath.Join("..", "shared-hooks")
	mainCopy := func(rel string) func(main, _ string) string {
		return func(main, _ string) string { return filepath.Join(main, rel) }
	}
	ownCopy := func(rel string) func(_, wt string) string {
		return func(_, wt string) string { return filepath.Join(wt, rel) }
	}
	for _, tc := range []struct {
		name string
		raw  string
		// track commits a hook under this directory before the worktree
		// is created.
		track string
		// anchored reports whether the value becomes absolute under the
		// main checkout.
		anchored  bool
		wantHooks func(main, wt string) string
	}{
		{
			name:      "untracked working tree dir uses main checkout",
			raw:       ".githooks",
			anchored:  true,
			wantHooks: mainCopy(".githooks"),
		},
		{
			name:      "git dir path uses main checkout",
			raw:       gitDirHooks,
			anchored:  true,
			wantHooks: mainCopy(gitDirHooks),
		},
		{
			name:      "outside path uses main checkout",
			raw:       outsideHooks,
			anchored:  true,
			wantHooks: mainCopy(outsideHooks),
		},
		{
			name:      "tracked dir stays per worktree",
			raw:       ".githooks",
			track:     ".githooks",
			wantHooks: ownCopy(".githooks"),
		},
		{
			name:      "unclean tracked dir stays per worktree",
			raw:       "./tools/../.githooks",
			track:     ".githooks",
			wantHooks: ownCopy(".githooks"),
		},
		{
			name: "tilde path is left for git",
			raw:  "~/hooks",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			ctx := t.Context()
			// Resolve symlinks (macOS /var -> /private/var) so paths
			// from Git and from the fixture compare equal.
			repo := gittest.NewRepo(t, gittest.Options{ConfigureUser: true, ResolvePath: true})
			repo.CommitFile(filepath.Join("sub", "a.txt"), "a\n", "initial")
			if tc.track != "" {
				repo.CommitFile(filepath.Join(tc.track, "pre-commit"), "#!/bin/sh\n", "add hooks")
			}
			main := repo.Root
			wt := repo.AddWorktree("feature")
			repo.Config("core.hooksPath", tc.raw)

			if tc.wantHooks != nil {
				got, err := HooksPath(ctx, wt)
				require.NoError(err)
				assert.Equal(t, tc.wantHooks(main, wt), got, "before normalization")
				got, err = HooksPath(ctx, filepath.Join(wt, "sub"))
				require.NoError(err)
				assert.Equal(t, tc.wantHooks(main, wt), got, "nested dir before normalization")
			}

			require.NoError(EnsureAbsoluteHooksPath(ctx, wt))
			want := tc.raw
			if tc.anchored {
				want = filepath.Join(main, tc.raw)
			}
			assert.Equal(t, want, repo.Run("config", "--local", "core.hooksPath"))

			if tc.wantHooks != nil {
				got, err := HooksPath(ctx, wt)
				require.NoError(err)
				assert.Equal(t, tc.wantHooks(main, wt), got, "linked worktree")
				got, err = HooksPath(ctx, filepath.Join(wt, "sub"))
				require.NoError(err)
				assert.Equal(t, tc.wantHooks(main, wt), got, "linked worktree nested dir")
				got, err = HooksPath(ctx, main)
				require.NoError(err)
				assert.Equal(t, tc.wantHooks(main, main), got, "main checkout")
			}
		})
	}
}

func TestHooksPathDefaultsToCommonGitDir(t *testing.T) {
	repo := gittest.NewRepoWithCommit(t)
	main, err := filepath.EvalSymlinks(repo.Root)
	require.NoError(t, err)
	wt := repo.AddWorktree("feature")

	got, err := HooksPath(t.Context(), wt)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(main, ".git", "hooks"), got)
}
