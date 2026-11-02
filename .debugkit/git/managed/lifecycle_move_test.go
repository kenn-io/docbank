package managedworktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestMoveWorktreeReturnsRollbackEvidence(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"", "topic"} {
		t.Run("branch="+branch, func(t *testing.T) {
			t.Parallel()
			root := initLifecycleRepo(t)
			source := filepath.Join(t.TempDir(), "prepared")
			destination := filepath.Join(t.TempDir(), "workspace")
			args := []string{"worktree", "add", "--detach", source, "HEAD"}
			if branch != "" {
				args = []string{"worktree", "add", "-b", branch, source, "HEAD"}
			}
			lifecycleGit(t, root, args...)
			require.NoError(t, os.WriteFile(filepath.Join(source, "retained"), []byte("prepared content"), 0o600))
			before, err := os.Stat(filepath.Join(source, "retained"))
			require.NoError(t, err)
			// Windows loads file identity lazily; capture it before the old path moves.
			require.True(t, os.SameFile(before, before))
			result, err := MoveWorktreeOnDisk(t.Context(), MoveWorktreeOptions{ProjectRoot: root, Source: source, Path: destination, Runner: lifecycleTestRunner(t)})
			require.NoError(t, err)
			require.Equal(t, destination, result.Path)
			require.Equal(t, branch, result.Branch)
			require.False(t, result.BranchCreated)
			after, err := os.Stat(filepath.Join(destination, "retained"))
			require.NoError(t, err)
			require.True(t, os.SameFile(before, after))
			_, err = result.Rollback(t.Context(), RollbackUnchanged)
			require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
			require.NoError(t, os.Remove(filepath.Join(destination, "retained")))
			_, err = result.Rollback(t.Context(), RollbackUnchanged)
			require.NoError(t, err)
			require.NoDirExists(t, destination)
			if branch != "" {
				require.True(t, branchExistsInRepo(t, root, branch))
			}
		})
	}
}

// Serialize acquisition capture after cancellation of a successful worktree move.
func TestMoveWorktreeCapturesSuccessfulMoveAfterCancellation(t *testing.T) {
	root := initLifecycleRepo(t)
	source := filepath.Join(t.TempDir(), "prepared")
	destination := filepath.Join(t.TempDir(), "workspace")
	lifecycleGit(t, root, "worktree", "add", "--detach", source, "HEAD")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := MoveWorktreeOnDisk(ctx, MoveWorktreeOptions{ProjectRoot: root, Source: source, Path: destination, Runner: lifecycleTestRunner(t), RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		out, err := runner.Output(ctx, dir, args...)
		if err == nil && strings.Join(args, " ") == "worktree move -- "+source+" "+destination {
			cancel()
			return out, nil
		}
		return out, err
	}})
	require.ErrorIs(t, err, context.Canceled)
	require.DirExists(t, destination)
	_, err = result.Rollback(t.Context(), RollbackUnchanged)
	require.NoError(t, err)
	require.NoDirExists(t, destination)
}

func TestMoveWorktreeRejectsForeignPrimaryAndOccupiedTargets(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"primary", "foreign", "occupied", "failed move"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := initLifecycleRepo(t)
			source := filepath.Join(t.TempDir(), "prepared")
			destination := filepath.Join(t.TempDir(), "workspace")
			lifecycleGit(t, root, "worktree", "add", "--detach", source, "HEAD")
			opts := MoveWorktreeOptions{ProjectRoot: root, Source: source, Path: destination, Runner: lifecycleTestRunner(t)}
			switch name {
			case "primary":
				opts.Source = root
			case "foreign":
				opts.ProjectRoot = initLifecycleRepo(t)
			case "occupied":
				require.NoError(t, os.Mkdir(destination, 0o700))
				t.Cleanup(func() { require.NoError(t, os.Remove(destination)) })
			case "failed move":
				opts.RunGit = func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
					if len(args) > 1 && args[0] == "worktree" && args[1] == "move" {
						return nil, errors.New("move unavailable")
					}
					return r.Output(ctx, dir, args...)
				}
			}
			result, err := MoveWorktreeOnDisk(t.Context(), opts)
			require.Error(t, err)
			if name == "failed move" {
				require.ErrorContains(t, err, "move unavailable")
			}
			_, err = result.Rollback(t.Context(), RollbackFreshOwned)
			require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
			require.DirExists(t, source)
			require.DirExists(t, root)
		})
	}
}

// Serialize new-branch capture and post-failure acquisition verification.
func TestMovePreparedWorktreeCreatesOwnedBranch(t *testing.T) {
	for _, outcome := range []string{"success", "move failure", "unknown move", "unknown move without new branch", "branch exists"} {
		t.Run(outcome, func(t *testing.T) {
			root := initLifecycleRepo(t)
			source := filepath.Join(t.TempDir(), "prepared")
			destination := filepath.Join(t.TempDir(), "workspace")
			lifecycleGit(t, root, "worktree", "add", "--detach", source, "HEAD")
			before, err := os.Stat(source)
			require.NoError(t, err)
			require.True(t, os.SameFile(before, before))
			failure := errors.New("move interrupted")
			if outcome == "branch exists" {
				lifecycleGit(t, root, "branch", "topic")
			}
			newBranch, baseRef := "topic", "HEAD"
			if outcome == "unknown move without new branch" {
				newBranch, baseRef = "", ""
			}
			result, err := MoveWorktreeOnDisk(t.Context(), MoveWorktreeOptions{
				ProjectRoot: root, Source: source, Path: destination, NewBranch: newBranch, BaseRef: baseRef, Runner: lifecycleTestRunner(t),
				RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
					moving := len(args) > 1 && args[0] == "worktree" && args[1] == "move"
					if moving && outcome == "move failure" {
						return nil, failure
					}
					out, err := runner.Output(ctx, dir, args...)
					if moving && err == nil && strings.HasPrefix(outcome, "unknown move") {
						return out, failure
					}
					return out, err
				},
			})
			if outcome == "branch exists" {
				require.ErrorIs(t, err, ErrBranchAlreadyExists)
				require.DirExists(t, source)
				require.Empty(t, result.Path)
				return
			}
			if outcome == "success" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, failure)
			}
			require.Equal(t, newBranch != "", result.BranchCreated)
			path := destination
			if outcome == "move failure" {
				path = source
			}
			require.Equal(t, path, result.Path)
			after, err := os.Stat(path)
			require.NoError(t, err)
			require.True(t, os.SameFile(before, after))
			policy := RollbackUnchanged
			if newBranch == "" {
				policy = RollbackFreshOwned
			}
			remaining, err := result.Rollback(t.Context(), policy)
			if strings.HasPrefix(outcome, "unknown move") {
				require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
				require.True(t, remaining.Unverified)
				require.DirExists(t, path)
				require.Equal(t, newBranch != "", branchExistsInRepo(t, root, "topic"))
				require.Equal(t, destination, remaining.Path)
			} else {
				require.NoError(t, err)
				require.NoDirExists(t, path)
				require.False(t, branchExistsInRepo(t, root, "topic"))
			}
		})
	}
}

// The project root must survive the move: Git runs from it to capture rollback
// evidence and later to roll back the moved checkout.
func TestMoveWorktreeRejectsProjectRootThatMovesWithSource(t *testing.T) {
	t.Parallel()
	for _, spelling := range []string{"same", "symlink"} {
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()
			root := initLifecycleRepo(t)
			source := filepath.Join(t.TempDir(), "prepared")
			lifecycleGit(t, root, "worktree", "add", "--detach", source, "HEAD")
			projectRoot := source
			if spelling == "symlink" {
				projectRoot = filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(source, projectRoot); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			destination := filepath.Join(t.TempDir(), "workspace")

			_, err := MoveWorktreeOnDisk(t.Context(), MoveWorktreeOptions{ProjectRoot: projectRoot, Source: source, Path: destination, Runner: lifecycleTestRunner(t)})

			require.ErrorIs(t, err, ErrInvalidWorktreeOptions)
			require.DirExists(t, source)
			require.NoDirExists(t, destination)
		})
	}
}

func TestMoveDeferredWorktreeRollsBackUntouchedCheckout(t *testing.T) {
	t.Parallel()
	root := initLifecycleRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "tracked"), []byte("tracked\n"), 0o600))
	lifecycleGit(t, root, "add", "tracked")
	lifecycleGit(t, root, "commit", "-q", "-m", "tracked file")
	source := filepath.Join(t.TempDir(), "prepared")
	lifecycleGit(t, root, "worktree", "add", "--detach", "--no-checkout", source, "HEAD")
	destination := filepath.Join(t.TempDir(), "workspace")
	result, err := MoveWorktreeOnDisk(t.Context(), MoveWorktreeOptions{ProjectRoot: root, Source: source, Path: destination, Runner: lifecycleTestRunner(t)})
	require.NoError(t, err)

	_, err = result.Rollback(t.Context(), RollbackUnchanged)

	require.NoError(t, err)
	require.NoDirExists(t, destination)
}
