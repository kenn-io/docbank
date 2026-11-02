package managedworktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestConfiguredPushUsesSelectedUpstream(t *testing.T) {
	for _, scope := range []UpstreamScope{UpstreamWorktree, UpstreamRepository} {
		t.Run(map[UpstreamScope]string{UpstreamWorktree: "worktree", UpstreamRepository: "repository"}[scope], func(t *testing.T) {
			isolateLifecycleGitConfig(t)
			_, clone := initOriginAndClone(t)
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
			})
			require.NoError(t, err)
			lifecycleGit(t, clone, "config", "extensions.worktreeConfig", "true")
			lifecycleGit(t, clone, "config", "--global", "push.default", "simple")
			lifecycleGit(t, clone, "config", "--local", "push.default", "current")
			lifecycleGit(t, created.Path, "config", "--worktree", "push.default", "current")
			other := filepath.Join(t.TempDir(), "other")
			lifecycleGit(t, clone, "branch", "--track", "other", "origin/main")
			lifecycleGit(t, clone, "worktree", "add", other, "other")
			err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: created.Path, Runner: lifecycleTestRunner(t),
				Policy: UpstreamPolicy{Action: UpstreamTrack, Scope: scope, Remote: "origin", Ref: "refs/heads/main", ConfigurePush: true},
			})
			require.NoError(t, err)
			push := lifecycleGit(t, created.Path, "push", "--dry-run", "--porcelain")
			assert.Contains(t, push, "refs/heads/topic:refs/heads/main")
			assert.NotContains(t, push, "refs/heads/topic:refs/heads/topic")
			assert.Contains(t, lifecycleGit(t, other, "push", "--dry-run", "--porcelain"), "refs/heads/other:refs/heads/other")
			_, err = RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
				ProjectRoot: clone, Path: created.Path, Branch: "topic", Runner: lifecycleTestRunner(t),
			})
			require.NoError(t, err)
			assert.Equal(t, "current", lifecycleGit(t, clone, "config", "--local", "push.default"))
			assert.Contains(t, lifecycleGit(t, other, "push", "--dry-run", "--porcelain"), "refs/heads/other:refs/heads/other")
		})
	}
}

func TestConfiguredPushRejectsCommandOverrideBeforeChangingRouting(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"runner", "environment"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			_, clone := initOriginAndClone(t)
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
			})
			require.NoError(t, err)
			runner := lifecycleTestRunner(t)
			if source == "runner" {
				runner = runner.WithConfig("push.default", "current")
			} else {
				runner.Env = append(runner.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=push.default", "GIT_CONFIG_VALUE_0=current")
			}
			before, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
			require.NoError(t, err)
			err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: created.Path, Runner: runner,
				Policy: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Remote: "origin", Ref: "refs/heads/main", ConfigurePush: true},
			})
			require.ErrorIs(t, err, ErrInvalidWorktreeOptions)
			after, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

// t.Setenv changes process-wide Git config, so this test stays serial.
func TestLifecycleExecutionOptionsPreserveInheritedConfig(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "fixture.value")
	t.Setenv("GIT_CONFIG_VALUE_0", "inherited")
	for _, tc := range []struct {
		name   string
		runner gitcmd.Runner
	}{
		{"wait delay", gitcmd.Runner{WaitDelay: time.Second}},
		{"stdout limit", gitcmd.Runner{StdoutLimit: 1 << 20}},
		{"stderr limit", gitcmd.Runner{StderrLimit: 1 << 20}},
		{"accept wait delay", gitcmd.Runner{AcceptSuccessfulWaitDelay: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initLifecycleRepo(t)
			observed := false
			_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: tc.runner,
				RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
					if !observed {
						out, err := runner.Output(ctx, dir, "config", "--get", "fixture.value")
						require.NoError(t, err)
						assert.Equal(t, "inherited\n", string(out))
						observed = true
					}
					out, stderr, err := runner.Run(ctx, dir, nil, args...)
					return append(out, stderr...), err
				},
			})
			require.NoError(t, err)
			assert.True(t, observed)
		})
	}
}

func TestCreateExplicitCheckoutModes(t *testing.T) {
	t.Parallel()
	for _, bare := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkout", true: "bare"}[bare], func(t *testing.T) {
			t.Parallel()
			root := initLifecycleRepo(t)
			require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("original\n"), 0o600))
			lifecycleGit(t, root, "add", "tracked.txt")
			lifecycleGit(t, root, "commit", "-m", "tracked content")
			if bare {
				clone := filepath.Join(t.TempDir(), "bare.git")
				lifecycleGit(t, root, "clone", "--bare", root, clone)
				root = clone
			}
			runner := lifecycleTestRunner(t)
			lifecycleGit(t, root, "branch", "existing")
			attached, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Branch: "existing", Path: filepath.Join(t.TempDir(), "attached"),
				Mode: CheckoutExistingBranch, Runner: runner,
			})
			require.NoError(t, err)
			assert.False(t, attached.BranchCreated)
			assert.Equal(t, "existing", lifecycleGit(t, attached.Path, "branch", "--show-current"))
			_, err = CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Branch: "missing", Path: filepath.Join(t.TempDir(), "missing"),
				Mode: CheckoutExistingBranch, Runner: runner,
			})
			require.ErrorIs(t, err, ErrBranchNotFound)
			assert.False(t, branchExistsInRepo(t, root, "missing"))
			_, err = CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Branch: "existing", Path: filepath.Join(t.TempDir(), "collision"),
				Mode: CheckoutNewBranch, Runner: runner,
			})
			require.ErrorIs(t, err, ErrBranchAlreadyExists)
			detached, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Path: filepath.Join(t.TempDir(), "detached"), BaseRef: "HEAD",
				Mode: CheckoutDetached, NoCheckout: true, LockReason: "fixture-preparing", Runner: runner,
			})
			require.NoError(t, err)
			assert.Empty(t, detached.Branch)
			assert.False(t, detached.BranchCreated)
			assert.Empty(t, lifecycleGit(t, detached.Path, "branch", "--show-current"))
			assert.NoFileExists(t, filepath.Join(detached.Path, "tracked.txt"))
			registration := lifecycleGit(t, detached.Path, "rev-parse", "--absolute-git-dir")
			marker, err := os.ReadFile(filepath.Join(registration, "locked"))
			require.NoError(t, err)
			assert.Equal(t, "fixture-preparing", strings.TrimSpace(string(marker)))
		})
	}
}

// Serialize worktree-add and snapshot failures around five-second acquisition capture.
func TestCreateDeferredFailureReportsAcquisition(t *testing.T) {
	for _, phase := range []string{"add", "snapshot", "checkout", "tracking", "hook"} {
		t.Run(phase, func(t *testing.T) {
			root := initLifecycleRepo(t)
			lifecycleGit(t, root, "remote", "add", "selected", root)
			path := filepath.Join(t.TempDir(), "checkout")
			failure := errors.New("fixture operation failed")
			failed := false
			hook := writeHookScript(t, root, filepath.Join(t.TempDir(), "hook-output"), 0)
			result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Branch: "topic", Path: path, Runner: lifecycleTestRunner(t),
				Mode: CheckoutNewBranch, Checkout: CheckoutIsolated, FailureCleanup: CleanupDeferred, SetupScript: hook,
				Upstream: UpstreamPolicy{Action: UpstreamTrack, Remote: "selected", Ref: "refs/heads/main"},
				RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
					out, stderr, err := runner.Run(ctx, dir, nil, args...)
					if !failed && ((phase == "add" && len(args) > 1 && args[0] == "worktree" && args[1] == "add") ||
						(phase == "snapshot" && dir == path && slices.Contains(args, "--absolute-git-dir")) ||
						(phase == "checkout" && slices.Contains(args, "reset")) ||
						(phase == "tracking" && slices.Contains(args, "--replace-all") && slices.Contains(args, "branch.topic.remote"))) {
						failed = true
						return append(out, stderr...), errors.Join(err, failure)
					}
					return append(out, stderr...), err
				},
				RunHook: func(ctx context.Context, command HookCommand) error {
					if phase == "hook" {
						return failure
					}
					return runTestHook(ctx, command)
				},
			})
			require.ErrorIs(t, err, failure)
			assert.Equal(t, path, result.Path)
			assert.Equal(t, "topic", result.Branch)
			assert.DirExists(t, path)
			assert.True(t, branchExistsInRepo(t, root, "topic"))
		})
	}
}

func TestPreparedHookRetainsValidationAndEnvironment(t *testing.T) {
	t.Parallel()
	root := initLifecycleRepo(t)
	path := filepath.Join(t.TempDir(), "worktree")
	output := filepath.Join(t.TempDir(), "hook-output")
	script := writeHookScript(t, root, output, 0)
	var hookDir string
	prepared, err := PrepareWorktreeHook(t.Context(), WorktreeHookOptions{
		ProjectRoot: root, Path: path, Branch: "topic", Script: script,
		WorktreeName: "display-name", RunHook: testHookRunnerRecordingDir(&hookDir),
	})
	require.NoError(t, err)
	assert.NoFileExists(t, output)
	_, err = CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: root, Path: path, Branch: "topic", Runner: lifecycleTestRunner(t),
	})
	require.NoError(t, err)
	require.NoError(t, prepared.Run(t.Context()))
	contents, err := os.ReadFile(output)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	require.Len(t, lines, 5)
	canonicalPath, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	if runtime.GOOS == "windows" {
		assert.Equal(t, path, hookDir, "hook runner receives the native worktree directory")
	} else {
		assert.Equal(t, canonicalPath, lines[0], "hook runs in the worktree directory")
	}
	assert.Equal(t, "name=display-name", lines[1])
	assert.Equal(t, "path="+path, lines[2])
	assert.Equal(t, "root="+root, lines[3])
	assert.Equal(t, "branch=topic", lines[4])
	_, err = PrepareWorktreeHook(t.Context(), WorktreeHookOptions{
		ProjectRoot: root, Path: path, Script: filepath.Join(filepath.Dir(root), "outside"),
	})
	require.ErrorIs(t, err, ErrHookOutsideProject)
	_, err = PrepareWorktreeHook(t.Context(), WorktreeHookOptions{
		ProjectRoot: root, Path: path, Script: filepath.Join(path, "hook"), MergeRequest: true,
	})
	require.Error(t, err)
}

func TestPreparedHookUsesCreationDestination(t *testing.T) {
	t.Parallel()
	for _, derived := range []bool{false, true} {
		t.Run(strconv.FormatBool(derived), func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			root := initLifecycleRepo(t)
			base := filepath.Join(t.TempDir(), "checkouts")
			path := filepath.Join(base, "feature-topic")
			output := filepath.Join(t.TempDir(), "hook-output")
			script := writeHookScript(t, root, output, 0)
			opts := WorktreeHookOptions{
				ProjectRoot: root, Path: path, Branch: "feature/topic", BaseDir: base,
				Script: script, RunHook: testHookRunner(),
			}
			if derived {
				opts.Path = ""
			}
			prepared, err := PrepareWorktreeHook(t.Context(), opts)
			require.NoError(err)
			require.NoDirExists(path)
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Path: prepared.WorktreePath(), Branch: opts.Branch,
				Runner: lifecycleTestRunner(t),
			})
			require.NoError(err)
			require.NoError(prepared.Run(t.Context()))
			contents, err := os.ReadFile(output)
			require.NoError(err)
			require.Contains(string(contents), created.Path)
			require.Equal("feature-topic", filepath.Base(created.Path))
		})
	}
}

func TestImportExplicitUpstreamPolicy(t *testing.T) {
	t.Parallel()
	for _, action := range []UpstreamAction{UpstreamLeave, UpstreamClear, UpstreamTrack} {
		t.Run(map[UpstreamAction]string{UpstreamLeave: "leave", UpstreamClear: "clear", UpstreamTrack: "track"}[action], func(t *testing.T) {
			t.Parallel()
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, clone, "remote", "add", "selected", origin)
			lifecycleGit(t, clone, "fetch", "selected")
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "new import head")
			lifecycleGit(t, clone, "config", "branch.topic.remote", "kept")
			lifecycleGit(t, clone, "config", "branch.topic.merge", "refs/heads/kept")
			result, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
				ProjectRoot: clone, Branch: "topic", Path: filepath.Join(t.TempDir(), "checkout"),
				Number: 7, HeadBranch: "main", HeadRepoCloneURL: origin, ProjectRepoIdentity: origin,
				Runner: lifecycleTestRunner(t), FailureCleanup: CleanupDeferred,
				Upstream: UpstreamPolicy{Action: action, Scope: UpstreamRepository, Remote: "selected", Ref: "refs/heads/main"},
			})
			require.NoError(t, err)
			assert.NotEqual(t, lifecycleGit(t, clone, "rev-parse", "selected/main"), lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
			switch action {
			case UpstreamDefault:
				require.FailNow(t, "test requires an explicit upstream action")
			case UpstreamLeave:
				assert.Equal(t, "kept", worktreeConfig(t, result.Path, "branch.topic.remote"))
				assert.Equal(t, "refs/heads/kept", worktreeConfig(t, result.Path, "branch.topic.merge"))
			case UpstreamClear:
				assert.Empty(t, worktreeConfig(t, result.Path, "branch.topic.remote"))
				assert.Empty(t, worktreeConfig(t, result.Path, "branch.topic.merge"))
			case UpstreamTrack:
				assert.Equal(t, "selected", lifecycleGit(t, clone, "config", "--local", "branch.topic.remote"))
				assert.Equal(t, "refs/heads/main", lifecycleGit(t, clone, "config", "--local", "branch.topic.merge"))
			}
		})
	}
}

func TestIsolatedCreationPreservesDefaultTracking(t *testing.T) {
	t.Parallel()
	origin, clone := initOriginAndClone(t)
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic",
		BaseRef: "origin/main", Checkout: CheckoutIsolated, Runner: lifecycleTestRunner(t),
	})
	require.NoError(t, err)
	assert.Equal(t, "refs/remotes/origin/main", lifecycleGit(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "remote update")
	want := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, created.Path, "pull", "--ff-only")
	assert.Equal(t, want, lifecycleGit(t, created.Path, "rev-parse", "HEAD"))
}

func TestUpstreamUsesWorktreeRemote(t *testing.T) {
	t.Parallel()
	for _, condition := range []TrackingCondition{TrackingExplicit, TrackingIfHeadMatches} {
		t.Run(map[TrackingCondition]string{TrackingExplicit: "explicit", TrackingIfHeadMatches: "matching"}[condition], func(t *testing.T) {
			t.Parallel()
			origin, clone := initOriginAndClone(t)
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
			})
			require.NoError(t, err)
			lifecycleGit(t, clone, "config", "extensions.worktreeConfig", "true")
			lifecycleGit(t, created.Path, "config", "--worktree", "remote.worktree-only.url", origin)
			lifecycleGit(t, created.Path, "config", "--worktree", "remote.worktree-only.fetch", "+refs/heads/*:refs/remotes/worktree-only/*")
			lifecycleGit(t, created.Path, "fetch", "worktree-only")
			err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: created.Path, Runner: lifecycleTestRunner(t),
				Policy: UpstreamPolicy{Action: UpstreamTrack, Condition: condition, Remote: "worktree-only", Ref: "refs/heads/main"},
			})
			require.NoError(t, err)
			assert.Equal(t, "refs/remotes/worktree-only/main", lifecycleGit(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "remote update")
			want := lifecycleGit(t, origin, "rev-parse", "HEAD")
			lifecycleGit(t, created.Path, "pull", "--ff-only")
			assert.Equal(t, want, lifecycleGit(t, created.Path, "rev-parse", "HEAD"))
		})
	}
}

func TestExplicitUpstreamScopeAndHeadCondition(t *testing.T) {
	t.Parallel()
	for _, condition := range []TrackingCondition{TrackingExplicit, TrackingIfHeadMatches} {
		t.Run(map[TrackingCondition]string{TrackingExplicit: "explicit", TrackingIfHeadMatches: "matching"}[condition], func(t *testing.T) {
			t.Parallel()
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, clone, "remote", "add", "selected", origin)
			lifecycleGit(t, clone, "config", "remote.selected.fetch", "+refs/heads/*:refs/tracked/selected/*")
			lifecycleGit(t, clone, "fetch", "selected")
			lifecycleGit(t, clone, "commit", "--allow-empty", "-m", "local advance")
			result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Branch: "topic", Path: filepath.Join(t.TempDir(), "checkout"),
				Runner:   lifecycleTestRunner(t),
				Upstream: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Condition: condition, Remote: "selected", Ref: "refs/heads/main"},
			})
			require.NoError(t, err)
			if condition == TrackingIfHeadMatches {
				assert.Empty(t, worktreeConfig(t, result.Path, "branch.topic.remote"))
				lifecycleGit(t, result.Path, "reset", "--hard", "refs/tracked/selected/main")
				err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
					ProjectRoot: clone, Path: result.Path, Runner: lifecycleTestRunner(t),
					Policy: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Condition: condition, Remote: "selected", Ref: "refs/heads/main"},
				})
				require.NoError(t, err)
			}
			assert.Equal(t, "selected", lifecycleGit(t, clone, "config", "--local", "branch.topic.remote"))
			assert.Equal(t, "refs/tracked/selected/main", lifecycleGit(t, result.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
			assert.Empty(t, worktreeConfig(t, result.Path, "branch.topic.pushRemote"))
			err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: result.Path, Runner: lifecycleTestRunner(t),
				Policy: UpstreamPolicy{Action: UpstreamClear, Scope: UpstreamRepository},
			})
			require.NoError(t, err)
			_, _, err = lifecycleTestRunner(t).Run(t.Context(), clone, nil, "config", "--local", "--get", "branch.topic.remote")
			require.True(t, gitcmd.IsExitCode(err, 1))
		})
	}
}

func TestImportExplicitCheckoutModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []CheckoutMode{CheckoutExistingBranch, CheckoutDetached} {
		t.Run(map[CheckoutMode]string{CheckoutExistingBranch: "attach", CheckoutDetached: "detached"}[mode], func(t *testing.T) {
			t.Parallel()
			origin, clone := initOriginAndClone(t)
			require.NoError(t, os.WriteFile(filepath.Join(origin, "tracked"), []byte("content"), 0o600))
			lifecycleGit(t, origin, "add", "tracked")
			lifecycleGit(t, origin, "commit", "-m", "tracked file")
			lifecycleGit(t, clone, "fetch", "origin")
			branch := ""
			if mode == CheckoutExistingBranch {
				branch = "existing"
				lifecycleGit(t, clone, "branch", branch, "origin/main")
			}
			result, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: branch,
				Mode: mode, NoCheckout: true, Number: 1, Runner: lifecycleTestRunner(t),
				HeadBranch: "main", HeadRepoCloneURL: origin, ProjectRepoIdentity: origin,
			})
			require.NoError(t, err)
			assert.False(t, result.BranchCreated)
			assert.Equal(t, branch, lifecycleGit(t, result.Path, "branch", "--show-current"))
			assert.NoFileExists(t, filepath.Join(result.Path, "tracked"))
			assert.Equal(t, lifecycleGit(t, origin, "rev-parse", "HEAD"), lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
		})
	}
}

func TestUpstreamScopeControlsSharedRouting(t *testing.T) {
	t.Parallel()
	for _, scope := range []UpstreamScope{UpstreamWorktree, UpstreamRepository} {
		t.Run(map[UpstreamScope]string{UpstreamWorktree: "worktree", UpstreamRepository: "repository"}[scope], func(t *testing.T) {
			t.Parallel()
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, origin, "checkout", "-b", "other")
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "other work")
			lifecycleGit(t, clone, "fetch", "origin")
			lifecycleGit(t, clone, "branch", "topic", "origin/other")
			lifecycleGit(t, clone, "config", "branch.topic.remote", "origin")
			lifecycleGit(t, clone, "config", "branch.topic.merge", "refs/heads/main")
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Mode: CheckoutExistingBranch,
				Runner:   lifecycleTestRunner(t),
				Upstream: UpstreamPolicy{Action: UpstreamTrack, Scope: scope, Remote: "origin", Ref: "refs/heads/other"},
			})
			if scope == UpstreamWorktree {
				require.ErrorIs(t, err, ErrInvalidWorktreeOptions)
				assert.Equal(t, "refs/remotes/origin/main", lifecycleGit(t, clone, "rev-parse", "--symbolic-full-name", "topic@{upstream}"))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "refs/remotes/origin/other", lifecycleGit(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
			lifecycleGit(t, created.Path, "pull", "--ff-only")
			require.NoError(t, SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: created.Path, Runner: lifecycleTestRunner(t),
				Policy: UpstreamPolicy{Action: UpstreamClear, Scope: scope},
			}))
			_, _, err = lifecycleTestRunner(t).Run(t.Context(), created.Path, nil, "rev-parse", "--verify", "@{upstream}")
			require.True(t, gitcmd.IsExitCode(err, 128), "clearing tracking must produce an ordinary no-upstream error: %v", err)
			assert.Equal(t, "refs/heads/main", worktreeConfig(t, clone, "branch.main.merge"))
		})
	}
}

func TestConditionalTrackingUsesRequestedTarget(t *testing.T) {
	t.Parallel()
	for _, matches := range []bool{false, true} {
		t.Run(map[bool]string{false: "mismatch", true: "match"}[matches], func(t *testing.T) {
			t.Parallel()
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, origin, "checkout", "-b", "other")
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "other work")
			lifecycleGit(t, clone, "fetch", "origin")
			start := "origin/main"
			if matches {
				start = "origin/other"
			}
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", BaseRef: start,
				Runner:   lifecycleTestRunner(t),
				Upstream: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Remote: "origin", Ref: "refs/heads/main"},
			})
			require.NoError(t, err)
			require.NoError(t, SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: created.Path, Runner: lifecycleTestRunner(t),
				Policy: UpstreamPolicy{Action: UpstreamTrack, Condition: TrackingIfHeadMatches, Scope: UpstreamRepository, Remote: "origin", Ref: "refs/heads/other"},
			}))
			want := "refs/remotes/origin/main"
			if matches {
				want = "refs/remotes/origin/other"
			}
			assert.Equal(t, want, lifecycleGit(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
		})
	}
}

func TestWorktreeUpstreamClearPreservesInheritedRouting(t *testing.T) {
	t.Parallel()
	origin, clone := initOriginAndClone(t)
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
		Upstream: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Remote: "origin", Ref: "refs/heads/main"},
	})
	require.NoError(t, err)
	require.ErrorIs(t, SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
		ProjectRoot: clone, Path: created.Path, Runner: lifecycleTestRunner(t),
		Policy: UpstreamPolicy{Action: UpstreamClear},
	}), ErrInvalidWorktreeOptions)
	assert.Equal(t, "refs/remotes/origin/main", lifecycleGit(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
	assert.Equal(t, origin, lifecycleGit(t, clone, "remote", "get-url", "origin"))
}

func TestUpstreamRefusesRoutingFromOtherConfigFiles(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"global", "include", "command"} {
		for _, action := range []UpstreamAction{UpstreamTrack, UpstreamClear} {
			t.Run(source+"/"+map[UpstreamAction]string{UpstreamTrack: "track", UpstreamClear: "clear"}[action], func(t *testing.T) {
				t.Parallel()
				_, clone := initOriginAndClone(t)
				external := filepath.Join(t.TempDir(), "external.gitconfig")
				contents := []byte("[branch \"topic\"]\nremote = origin\nmerge = refs/heads/main\n")
				require.NoError(t, os.WriteFile(external, contents, 0o600))
				runner := lifecycleTestRunner(t)
				switch source {
				case "global":
					runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+external)
				case "include":
					lifecycleGit(t, clone, "config", "include.path", external)
				case "command":
					runner = runner.WithConfig("branch.topic.remote", "origin").WithConfig("branch.topic.merge", "refs/heads/main")
				}
				created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
					ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: runner,
					Upstream: UpstreamPolicy{Action: UpstreamLeave},
				})
				require.NoError(t, err)
				before, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
				require.NoError(t, err)
				err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
					ProjectRoot: clone, Path: created.Path, Runner: runner,
					Policy: UpstreamPolicy{Action: action, Remote: "origin", Ref: "refs/heads/main"},
				})
				require.ErrorIs(t, err, ErrInvalidWorktreeOptions)
				after, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
				require.NoError(t, err)
				assert.Equal(t, before, after)
				after, err = os.ReadFile(external)
				require.NoError(t, err)
				assert.Equal(t, contents, after)
			})
		}
	}
}

func TestWorktreeTrackingPreservesExistingBranchUpstream(t *testing.T) {
	t.Parallel()
	_, clone := initOriginAndClone(t)
	lifecycleGit(t, clone, "branch", "--track", "topic", "origin/main")
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic",
		Mode: CheckoutExistingBranch, Runner: lifecycleTestRunner(t),
		Upstream: UpstreamPolicy{Action: UpstreamTrack, Remote: "origin", Ref: "refs/heads/main", ConfigurePush: true},
	})
	require.NoError(t, err)
	assert.Equal(t, "refs/remotes/origin/main", lifecycleGit(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
	lifecycleGit(t, created.Path, "pull", "--ff-only")
	_, err = RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{ProjectRoot: clone, Path: created.Path, Branch: "topic", Runner: lifecycleTestRunner(t)})
	require.NoError(t, err)
	assert.Equal(t, "refs/remotes/origin/main", lifecycleGit(t, clone, "rev-parse", "--symbolic-full-name", "topic@{upstream}"))
	assert.Equal(t, "refs/heads/main", lifecycleGit(t, clone, "config", "--local", "branch.topic.merge"))
}

func TestForkImportFetchesExplicitConditionalUpstream(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"matching", "stale", "mismatched", "leave", "clear", "branch config", "branch config deferred"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			origin, clone := initOriginAndClone(t)
			fork := filepath.Join(t.TempDir(), "fork")
			lifecycleGit(t, clone, "clone", "-q", origin, fork)
			lifecycleGit(t, fork, "config", "user.name", "Tester")
			lifecycleGit(t, fork, "config", "user.email", "test@example.invalid")
			lifecycleGit(t, fork, "checkout", "-b", "contribution")
			lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "contribution")
			head := lifecycleGit(t, fork, "rev-parse", "HEAD")
			lifecycleGit(t, origin, "fetch", fork, "+refs/heads/contribution:refs/pull/9/head")
			lifecycleGit(t, clone, "remote", "add", "fork", fork)
			// The requested tracking target differs from the inferred fork route.
			selected := filepath.Join(t.TempDir(), "selected")
			lifecycleGit(t, clone, "clone", "-q", fork, selected)
			start := head
			if scenario == "mismatched" {
				start = head + "^"
			}
			lifecycleGit(t, selected, "branch", "selected-topic", start)
			if strings.HasPrefix(scenario, "branch config") {
				external := filepath.Join(t.TempDir(), "tracking.config")
				lifecycleGit(t, clone, "config", "--file", external, "remote.selected.url", selected)
				lifecycleGit(t, clone, "config", "--file", external, "remote.selected.fetch", "+refs/heads/*:refs/tracked/selected/*")
				lifecycleGit(t, clone, "config", "includeIf.onbranch:topic.path", external)
			} else {
				lifecycleGit(t, clone, "remote", "add", "selected", selected)
				lifecycleGit(t, clone, "config", "remote.selected.fetch", "+refs/heads/*:refs/tracked/selected/*")
			}
			if scenario == "stale" {
				lifecycleGit(t, clone, "update-ref", "refs/tracked/selected/selected-topic", "HEAD")
			}
			action := UpstreamTrack
			switch scenario {
			case "leave":
				action = UpstreamLeave
			case "clear":
				action = UpstreamClear
			}
			created, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
				Number: 9, HeadBranch: "contribution", HeadRepoCloneURL: fork, ProjectRepoIdentity: origin,
				NoCheckout: scenario == "branch config deferred",
				Upstream:   UpstreamPolicy{Action: action, Condition: TrackingIfHeadMatches, Remote: "selected", Ref: "refs/heads/selected-topic"},
			})
			require.NoError(t, err)
			assert.Equal(t, head, lifecycleGit(t, created.Path, "rev-parse", "HEAD"))
			if scenario == "matching" || scenario == "stale" || strings.HasPrefix(scenario, "branch config") {
				assert.Equal(t, "refs/tracked/selected/selected-topic", lifecycleGit(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
				assert.Equal(t, head, lifecycleGit(t, created.Path, "rev-parse", "@{upstream}"))
			} else {
				_, _, err = lifecycleTestRunner(t).Run(t.Context(), created.Path, nil, "rev-parse", "--verify", "@{upstream}")
				assert.True(t, gitcmd.IsExitCode(err, 128), "no tracking for a mismatch, Leave or Clear")
			}
			if action != UpstreamTrack {
				_, _, err = lifecycleTestRunner(t).Run(t.Context(), clone, nil, "show-ref", "--verify", "--quiet", "refs/tracked/selected/selected-topic")
				assert.True(t, gitcmd.IsExitCode(err, 1), "Leave and Clear must not fetch the requested tracking target")
			}
		})
	}
}

// Serialize output truncation and cancellation around acquisition capture.
func TestCreateAcquisitionFailureRetainsCleanupEvidence(t *testing.T) {
	for _, failure := range []string{"output limit", "after add", "during snapshot"} {
		for _, cleanup := range []FailureCleanup{CleanupAutomatic, CleanupDeferred} {
			t.Run(failure+"/"+map[FailureCleanup]string{CleanupAutomatic: "automatic", CleanupDeferred: "deferred"}[cleanup], func(t *testing.T) {
				root := initLifecycleRepo(t)
				path := filepath.Join(t.TempDir(), "checkout")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				runner := lifecycleTestRunner(t)
				hook := filepath.Join(root, ".git", "hooks", "post-checkout")
				if failure == "output limit" {
					require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s' '"+strings.Repeat("x", 200000)+"' >&2\n"), 0o700))
					runner.StderrLimit = 4096
				}
				added := false
				created, err := CreateWorktreeOnDisk(ctx, CreateWorktreeOptions{
					ProjectRoot: root, Path: path, Branch: "topic", Runner: runner, FailureCleanup: cleanup,
					RunGit: func(commandCtx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
						if added && failure == "during snapshot" {
							cancel()
						}
						out, stderr, err := runner.Run(commandCtx, dir, nil, args...)
						if err == nil && len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
							added = true
							if failure == "after add" {
								cancel()
							}
						}
						return append(out, stderr...), err
					},
				})
				if failure == "output limit" {
					require.ErrorIs(t, err, gitcmd.ErrStderrLimitExceeded)
				} else {
					require.True(t, added)
					require.ErrorIs(t, err, context.Canceled)
				}
				require.NotErrorIs(t, err, ErrWorktreeCleanupIncomplete)
				if cleanup == CleanupDeferred {
					require.DirExists(t, path)
					_, err = created.Rollback(t.Context())
					require.NoError(t, err)
				}
				assert.NoDirExists(t, path)
				_, _, err = runner.Run(t.Context(), root, nil, "show-ref", "--verify", "--quiet", "refs/heads/topic")
				require.True(t, gitcmd.IsExitCode(err, 1))
				if failure == "output limit" {
					require.NoError(t, os.Remove(hook))
				}
				_, err = CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
					ProjectRoot: root, Path: path, Branch: "topic", Runner: runner,
				})
				require.NoError(t, err, "creation can be retried after failed acquisition is cleaned up")
			})
		}
	}
}

func TestConditionalImportFetchFailureUsesCleanupPolicy(t *testing.T) {
	t.Parallel()
	for _, cleanup := range []FailureCleanup{CleanupAutomatic, CleanupDeferred} {
		t.Run(map[FailureCleanup]string{CleanupAutomatic: "automatic", CleanupDeferred: "deferred"}[cleanup], func(t *testing.T) {
			t.Parallel()
			origin, clone := initOriginAndClone(t)
			path := filepath.Join(t.TempDir(), "checkout")
			created, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
				ProjectRoot: clone, Path: path, Branch: "topic", Runner: lifecycleTestRunner(t), FailureCleanup: cleanup,
				Number: 1, HeadBranch: "main", HeadRepoCloneURL: origin, ProjectRepoIdentity: origin, NoCheckout: true,
				Upstream: UpstreamPolicy{Action: UpstreamTrack, Condition: TrackingIfHeadMatches, Remote: "origin", Ref: "refs/heads/missing"},
			})
			require.True(t, gitcmd.IsExitCode(err, 128), "fetching a missing remote branch must fail: %v", err)
			require.NotErrorIs(t, err, ErrWorktreeCleanupIncomplete)
			if cleanup == CleanupDeferred {
				require.DirExists(t, path)
				_, err = created.Rollback(t.Context())
				require.NoError(t, err)
			}
			assert.NoDirExists(t, path)
			_, _, err = lifecycleTestRunner(t).Run(t.Context(), clone, nil, "show-ref", "--verify", "--quiet", "refs/heads/topic")
			require.True(t, gitcmd.IsExitCode(err, 1))
		})
	}
}
