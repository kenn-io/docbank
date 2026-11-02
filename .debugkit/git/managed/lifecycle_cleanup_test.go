package managedworktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fslink"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestDeferredCheckoutRollbackPreservesChanges(t *testing.T) {
	t.Parallel()
	for _, checkout := range []CheckoutPolicy{CheckoutTrusted, CheckoutIsolated} {
		for _, state := range []string{"untouched", "inspected", "untracked", "ignored", "empty-directory", "staged-only", "staged-deletion", "materialized", "edited"} {
			t.Run(map[CheckoutPolicy]string{CheckoutTrusted: "native", CheckoutIsolated: "isolated"}[checkout]+"/"+state, func(t *testing.T) {
				t.Parallel()
				root := initLifecycleRepo(t)
				require.NoError(t, os.WriteFile(filepath.Join(root, "tracked"), []byte("original\n"), 0o600))
				lifecycleGit(t, root, "add", ".")
				lifecycleGit(t, root, "commit", "-m", "tracked file")
				created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
					ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic",
					NoCheckout: true, Checkout: checkout, Runner: lifecycleTestRunner(t),
				})
				require.NoError(t, err)
				registration := lifecycleGit(t, created.Path, "rev-parse", "--absolute-git-dir")
				note := filepath.Join(created.Path, "note")
				switch state {
				case "inspected":
					lifecycleGit(t, created.Path, "status", "--porcelain")
				case "untracked", "ignored", "staged-only":
					require.NoError(t, os.WriteFile(note, []byte("preserve\n"), 0o600))
					switch state {
					case "ignored":
						require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("note\n"), 0o600))
					case "staged-only":
						lifecycleGit(t, created.Path, "add", "note")
						require.NoError(t, os.Remove(note))
					}
				case "empty-directory":
					require.NoError(t, os.Mkdir(note, 0o755))
				case "staged-deletion":
					lifecycleGit(t, created.Path, "read-tree", "--empty")
				case "materialized", "edited":
					lifecycleGit(t, created.Path, "reset", "--hard", "HEAD")
					if state == "edited" {
						require.NoError(t, os.WriteFile(filepath.Join(created.Path, "tracked"), []byte("preserve\n"), 0o600))
					}
				}
				remaining, err := created.Rollback(t.Context())
				if state == "untouched" || state == "inspected" || state == "materialized" {
					require.NoError(t, err)
					assert.Empty(t, remaining)
					assert.NoDirExists(t, created.Path)
					assert.NoDirExists(t, registration)
					assert.False(t, branchExistsInRepo(t, root, "topic"))
					return
				}
				require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
				assert.Equal(t, created.Path, remaining.Path)
				assert.DirExists(t, registration)
				assert.True(t, branchExistsInRepo(t, root, "topic"))
				switch state {
				case "untracked", "ignored":
					contents, err := os.ReadFile(note)
					require.NoError(t, err)
					assert.Equal(t, "preserve\n", string(contents))
				case "empty-directory":
					assert.DirExists(t, note)
				case "staged-only":
					assert.Equal(t, "preserve", lifecycleGit(t, created.Path, "show", ":note"))
				case "staged-deletion":
					assert.Equal(t, "D\ttracked", lifecycleGit(t, created.Path, "diff", "--cached", "--name-status"))
				case "edited":
					contents, err := os.ReadFile(filepath.Join(created.Path, "tracked"))
					require.NoError(t, err)
					assert.Equal(t, "preserve\n", string(contents))
				}
			})
		}
	}
}

// Serialize branch-existence inspection after canceled or output-limited deletion.
func TestCleanupReportsBranchDeletedBeforeExecutionError(t *testing.T) {
	for _, failure := range []string{"output-limit", "cancellation"} {
		for _, rollback := range []bool{false, true} {
			t.Run(failure+"/"+map[bool]string{false: "remove", true: "rollback"}[rollback], func(t *testing.T) {
				root := initLifecycleRepo(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				wantErr := gitcmd.ErrStdoutLimitExceeded
				if failure == "cancellation" {
					wantErr = context.Canceled
				}
				runGit := func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
					deleting := len(args) > 1 && args[0] == "branch" && args[1] == "-D"
					if deleting && failure == "output-limit" {
						runner.StdoutLimit = 1
					}
					out, stderr, err := runner.Run(ctx, dir, nil, args...)
					if deleting && failure == "cancellation" && err == nil {
						cancel()
						err = ctx.Err()
					}
					return append(out, stderr...), err
				}
				created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
					ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t), RunGit: runGit,
				})
				require.NoError(t, err)
				if rollback {
					remaining, err := created.Rollback(ctx, RollbackFreshOwned)
					require.ErrorIs(t, err, wantErr)
					assert.Empty(t, remaining)
				} else {
					lifecycleGit(t, root, "branch", "later")
					result, err := RemoveWorktreeFromDisk(ctx, RemoveWorktreeOptions{
						ProjectRoot: root, Path: created.Path, Branch: "topic", Force: true, Runner: lifecycleTestRunner(t), RunGit: runGit,
						Branches: []BranchRemoval{{Name: "topic", Force: true}, {Name: "later", Force: true}},
					})
					require.ErrorIs(t, err, wantErr)
					assert.Equal(t, []string{"topic"}, result.BranchesRemoved)
					assert.Equal(t, []string{"later"}, result.BranchesRemaining)
					assert.Equal(t, "later", result.Remaining.Branch)
					assert.True(t, branchExistsInRepo(t, root, "later"))
				}
				assert.NoDirExists(t, created.Path)
				assert.False(t, branchExistsInRepo(t, root, "topic"))
			})
		}
	}
}

func TestRemoveMissingWorktreeThroughLinkedParent(t *testing.T) {
	t.Parallel()
	root := initLifecycleRepo(t)
	base := t.TempDir()
	actual := filepath.Join(base, "actual")
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Mkdir(actual, 0o700))
	_, err := fslink.LinkDir(actual, alias)
	require.NoError(t, err)
	path := filepath.Join(alias, "checkout")
	lifecycleGit(t, root, "worktree", "add", "-b", "topic", path)
	registration := filepath.Clean(lifecycleGit(t, path, "rev-parse", "--absolute-git-dir"))
	require.NoError(t, os.RemoveAll(path))
	result, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot: root, Path: path, Branch: "topic", RemoveBranch: true, Runner: lifecycleTestRunner(t),
	})
	require.NoError(t, err)
	assert.True(t, result.RegistrationRemoved)
	assert.NoDirExists(t, registration)
	assert.False(t, branchExistsInRepo(t, root, "topic"))
}

func TestRollbackPoliciesPreserveUnownedArtifacts(t *testing.T) {
	t.Parallel()
	for _, policy := range []RollbackPolicy{RollbackUnchanged, RollbackFreshOwned} {
		for _, replacement := range []string{"directory", "repository", "registration", "unverified"} {
			t.Run(map[RollbackPolicy]string{RollbackUnchanged: "unchanged", RollbackFreshOwned: "fresh"}[policy]+"/"+replacement, func(t *testing.T) {
				t.Parallel()
				root := initLifecycleRepo(t)
				path := filepath.Join(t.TempDir(), "checkout")
				result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
					ProjectRoot: root, Path: path, Branch: "topic", Runner: lifecycleTestRunner(t),
					FailureCleanup: CleanupDeferred,
					RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
						stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
						if replacement == "unverified" && slices.Contains(args, "--absolute-git-dir") {
							return stdout, errors.New("fixture evidence failure")
						}
						return append(stdout, stderr...), err
					},
				})
				if replacement == "unverified" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				oid := lifecycleGit(t, root, "rev-parse", "topic")
				switch replacement {
				case "directory":
					require.NoError(t, os.Rename(path, path+"-original"))
					require.NoError(t, os.Mkdir(path, 0o700))
				case "repository":
					require.NoError(t, os.Rename(path, path+"-original"))
					other := initLifecycleRepo(t)
					require.NoError(t, os.Rename(other, path))
				case "registration":
					registration := lifecycleGit(t, path, "rev-parse", "--absolute-git-dir")
					require.NoError(t, os.Rename(registration, registration+"-original"))
					require.NoError(t, os.CopyFS(registration, os.DirFS(registration+"-original")))
				}
				remaining, err := result.Rollback(t.Context(), policy)
				require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
				assert.True(t, remaining.Unverified)
				assert.Equal(t, path, remaining.Path)
				assert.DirExists(t, path)
				assert.Equal(t, oid, lifecycleGit(t, root, "rev-parse", "topic"))
			})
		}
	}
}

func TestFreshRollbackDeletesOnlyAcquiredBranch(t *testing.T) {
	t.Parallel()
	root := initLifecycleRepo(t)
	lifecycleGit(t, root, "branch", "keep")
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "owned", Runner: lifecycleTestRunner(t),
	})
	require.NoError(t, err)
	path := result.Path
	lifecycleGit(t, path, "checkout", "keep")
	require.NoError(t, os.WriteFile(filepath.Join(path, "setup-output"), []byte("partial setup"), 0o600))
	result.Path, result.Branch, result.BranchCreated = root, "main", false
	remaining, err := result.Rollback(t.Context(), RollbackFreshOwned)
	require.NoError(t, err)
	assert.Empty(t, remaining)
	assert.NoDirExists(t, path)
	assert.False(t, branchExistsInRepo(t, root, "owned"))
	assert.True(t, branchExistsInRepo(t, root, "keep"))
	assert.DirExists(t, root)
}

func TestBranchCleanupWaitsForCheckoutRemoval(t *testing.T) {
	t.Parallel()
	root := initLifecycleRepo(t)
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
	})
	require.NoError(t, err)
	failure := errors.New("fixture remove failure")
	result, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot: root, Path: created.Path, Branch: "topic", Force: true, Runner: lifecycleTestRunner(t),
		Branches: []BranchRemoval{{Name: "topic", Force: true}},
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
				return nil, failure
			}
			stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), err
		},
	})
	require.ErrorIs(t, err, failure)
	assert.False(t, result.CheckoutRemoved)
	assert.False(t, result.RegistrationRemoved)
	assert.Empty(t, result.BranchesRemoved)
	assert.DirExists(t, created.Path)
	assert.True(t, branchExistsInRepo(t, root, "topic"))
}

func TestRemovalReportsPartialArtifacts(t *testing.T) {
	t.Parallel()
	root := initLifecycleRepo(t)
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
	})
	require.NoError(t, err)
	failure := errors.New("fixture residual directory")
	result, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot: root, Path: created.Path, Branch: "topic", Force: true, Runner: lifecycleTestRunner(t),
		Branches: []BranchRemoval{{Name: "topic", Force: true}},
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
			if err == nil && len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
				err = errors.Join(failure, os.Mkdir(created.Path, 0o700))
			}
			return append(stdout, stderr...), err
		},
	})
	require.ErrorIs(t, err, failure)
	assert.False(t, result.CheckoutRemoved)
	assert.True(t, result.RegistrationRemoved)
	assert.Empty(t, result.BranchesRemoved)
	assert.Equal(t, created.Path, result.Remaining.Path)
	assert.Equal(t, "topic", result.Remaining.Branch)
	assert.Empty(t, result.Remaining.Registration)
	assert.DirExists(t, created.Path)
	assert.True(t, branchExistsInRepo(t, root, "topic"))
}

func TestRemovalBranchPolicy(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"merged-only", "force", "changed-oid"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			root := initLifecycleRepo(t)
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
			})
			require.NoError(t, err)
			old := lifecycleGit(t, created.Path, "rev-parse", "HEAD")
			lifecycleGit(t, created.Path, "commit", "--allow-empty", "-m", "new work")
			head := lifecycleGit(t, created.Path, "rev-parse", "HEAD")
			expected := head
			if kind == "changed-oid" {
				expected = old
			}
			result, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
				ProjectRoot: root, Path: created.Path, Branch: "topic", Runner: lifecycleTestRunner(t),
				Branches: []BranchRemoval{{Name: "topic", Force: kind != "merged-only", ExpectedOID: expected}},
			})
			assert.True(t, result.CheckoutRemoved)
			assert.True(t, result.RegistrationRemoved)
			if kind == "force" {
				require.NoError(t, err)
				assert.Equal(t, []string{"topic"}, result.BranchesRemoved)
				assert.False(t, branchExistsInRepo(t, root, "topic"))
			} else {
				require.Error(t, err)
				assert.Empty(t, result.BranchesRemoved)
				assert.Equal(t, head, lifecycleGit(t, root, "rev-parse", "topic"))
			}
		})
	}
}

func TestRollbackPoliciesAfterHeadChanges(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"unstaged", "staged", "advanced-branch", "detached", "attached"} {
		for _, policy := range []RollbackPolicy{RollbackUnchanged, RollbackFreshOwned} {
			if kind == "unstaged" && policy == RollbackFreshOwned {
				continue
			}
			t.Run(kind+"/"+map[RollbackPolicy]string{RollbackUnchanged: "unchanged", RollbackFreshOwned: "fresh"}[policy], func(t *testing.T) {
				t.Parallel()
				root := initLifecycleRepo(t)
				if kind == "attached" {
					lifecycleGit(t, root, "branch", "topic")
				}
				created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
					ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
				})
				require.NoError(t, err)
				switch kind {
				case "unstaged", "staged":
					require.NoError(t, os.WriteFile(filepath.Join(created.Path, "staged"), []byte("work"), 0o600))
					if kind == "staged" {
						lifecycleGit(t, created.Path, "add", "staged")
					}
				case "advanced-branch":
					require.NoError(t, os.WriteFile(filepath.Join(created.Path, "review.txt"), []byte("keep\n"), 0o644))
					lifecycleGit(t, created.Path, "add", "review.txt")
					lifecycleGit(t, created.Path, "commit", "-m", "work")
				case "detached":
					lifecycleGit(t, created.Path, "checkout", "--detach")
					lifecycleGit(t, created.Path, "commit", "--allow-empty", "-m", "detached work")
				}
				head := lifecycleGit(t, root, "rev-parse", "topic")
				checkoutHead := lifecycleGit(t, created.Path, "rev-parse", "HEAD")
				registration := filepath.Clean(lifecycleGit(t, created.Path, "rev-parse", "--absolute-git-dir"))
				remaining, err := created.Rollback(t.Context(), policy)
				if policy == RollbackUnchanged && kind != "attached" {
					require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
					assert.Equal(t, RollbackResult{Path: created.Path, Branch: created.Branch, Registration: registration}, remaining)
					assert.Equal(t, checkoutHead, lifecycleGit(t, created.Path, "rev-parse", "HEAD"))
					assert.Equal(t, head, lifecycleGit(t, root, "rev-parse", "topic"))
					if kind == "advanced-branch" {
						assert.FileExists(t, filepath.Join(created.Path, "review.txt"))
					}
					if kind == "unstaged" || kind == "staged" {
						assert.FileExists(t, filepath.Join(created.Path, "staged"))
					}
				} else if kind == "advanced-branch" {
					require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
					assert.Empty(t, remaining.Path)
					assert.Equal(t, "topic", remaining.Branch)
					assert.Equal(t, head, lifecycleGit(t, root, "rev-parse", "topic"))
				} else {
					require.NoError(t, err)
					assert.Empty(t, remaining)
					assert.NoDirExists(t, created.Path)
					assert.Equal(t, kind == "attached", branchExistsInRepo(t, root, "topic"))
				}
			})
		}
	}
}

func TestPreparedHookRetainsExitCause(t *testing.T) {
	t.Parallel()
	root := initLifecycleRepo(t)
	script := writeHookScript(t, root, filepath.Join(t.TempDir(), "hook-output"), 7)
	hook, err := PrepareWorktreeHook(t.Context(), WorktreeHookOptions{
		ProjectRoot: root, Path: root, Script: script, RunHook: testHookRunner(),
	})
	require.NoError(t, err)
	err = hook.Run(t.Context())
	var classified *HookError
	require.ErrorAs(t, err, &classified)
	var process *exec.ExitError
	require.ErrorAs(t, err, &process)
	assert.Equal(t, 7, process.ExitCode())
	assert.Equal(t, 7, classified.ExitCode)
}

func TestRollbackKeepsEvidenceWhenRemovalPreflightFails(t *testing.T) {
	t.Parallel()
	root := initLifecycleRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rollingBack := false
	created, err := CreateWorktreeOnDisk(ctx, CreateWorktreeOptions{
		ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			if rollingBack && len(args) != 0 && args[0] == "check-ref-format" {
				cancel()
			}
			stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), err
		},
	})
	require.NoError(t, err)
	registration := filepath.Clean(lifecycleGit(t, created.Path, "rev-parse", "--absolute-git-dir"))
	rollingBack = true
	remaining, err := created.Rollback(ctx, RollbackFreshOwned)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
	assert.Equal(t, created.Path, remaining.Path)
	assert.Equal(t, "topic", remaining.Branch)
	assert.Equal(t, registration, remaining.Registration)
	assert.DirExists(t, created.Path)
	assert.DirExists(t, registration)
	assert.True(t, branchExistsInRepo(t, root, "topic"))
}

// Serialize branch-existence inspection after Git rejects deleting a checked-out branch.
func TestRemoveBranchAfterRegistrationCleanup(t *testing.T) {
	for _, state := range []string{"unused", "checked-out", "changed"} {
		t.Run(state, func(t *testing.T) {
			require := require.New(t)
			root := initLifecycleRepo(t)
			head := lifecycleGit(t, root, "rev-parse", "HEAD")
			lifecycleGit(t, root, "branch", "topic")
			switch state {
			case "checked-out":
				lifecycleGit(t, root, "worktree", "add", filepath.Join(t.TempDir(), "topic"), "topic")
			case "changed":
				lifecycleGit(t, root, "commit", "--allow-empty", "-m", "advance")
				lifecycleGit(t, root, "branch", "-f", "topic", "HEAD")
			}
			removed, err := RemoveBranch(t.Context(), RemoveBranchOptions{ProjectRoot: root, Branch: BranchRemoval{Name: "topic", Force: true, ExpectedOID: head}, Runner: lifecycleTestRunner(t)})
			if state == "unused" {
				require.NoError(err)
				require.True(removed)
				require.False(branchExistsInRepo(t, root, "topic"))
			} else {
				require.Error(err)
				require.False(removed)
				require.True(branchExistsInRepo(t, root, "topic"))
			}
		})
	}
}
