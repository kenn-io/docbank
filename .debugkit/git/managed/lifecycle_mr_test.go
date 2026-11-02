package managedworktree

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitcmd "go.kenn.io/kit/git/cmd"
)

func lifecycleGitCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = lifecycleGitEnv(t)
	return cmd
}

func identityOfCloneURL(rawURL string) string {
	return CloneURLIdentity(rawURL)
}

// initOriginAndClone builds an "origin" repository with one commit on main
// and a clone of it, returning (originDir, cloneDir). The clone is the
// project checkout merge-request worktrees are created in.
func initOriginAndClone(t *testing.T) (string, string) {
	t.Helper()
	origin := initLifecycleRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	lifecycleGit(t, filepath.Dir(origin), "clone", "-q", origin, clone)
	lifecycleGit(t, clone, "config", "user.email", "t@e.st")
	lifecycleGit(t, clone, "config", "user.name", "Tester")
	return origin, clone
}

func worktreeConfig(t *testing.T, dir, key string) string {
	t.Helper()
	cmd := lifecycleGitCommand(t, dir, "config", "--get", key)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func worktreeOnlyConfig(t *testing.T, dir, key string) string {
	t.Helper()
	cmd := lifecycleGitCommand(t, dir, "config", "--worktree", "--get", key)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func expectedSafeMergeDriverCommand(t *testing.T, worktree string) string {
	t.Helper()
	path, err := resolveMergeDriverGitPath()
	require.NoError(t, err)
	hooksPath := worktreeConfig(t, worktree, "core.hooksPath")
	require.NotEmpty(t, hooksPath)
	return safeMergeDriverCommand(path, hooksPath)
}

// TestCreateWorktreeFromMergeRequestSameRepo covers the same-repo scenario:
// the head branch is fetched from origin, the new local branch starts at
// it, and upstream tracking points at origin's head branch.
func TestCreateWorktreeFromMergeRequestSameRepo(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "feature-x")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "feature-x")
	lifecycleGit(t, origin, "checkout", "-q", "main")
	lifecycleGit(t, clone, "config", "remote.pushDefault", "elsewhere")
	lifecycleGit(t, clone, "tag", "origin/feature-x", "main")

	dest := filepath.Join(t.TempDir(), "wt")
	result, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-42",
			Path:                dest,
			Number:              42,
			HeadBranch:          "feature-x",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
		})
	require.NoError(err)
	assert.Equal(dest, result.Path)
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"),
		"worktree starts at the merge request head")
	assert.Equal("pr-42",
		lifecycleGit(t, dest, "rev-parse", "--abbrev-ref", "HEAD"))
	assert.Equal("origin", worktreeConfig(t, dest, "branch.pr-42.remote"))
	assert.Equal("refs/heads/feature-x",
		worktreeConfig(t, dest, "branch.pr-42.merge"))
	assert.Equal("origin",
		worktreeConfig(t, dest, "branch.pr-42.pushRemote"))
	assert.Equal("upstream", worktreeConfig(t, dest, "push.default"))
}

func TestCreateWorktreeFromMergeRequestUsesExplicitProjectRemote(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, clone, "remote", "rename", "origin", "upstream")
	lifecycleGit(t, origin, "checkout", "-q", "-b", "remote-feature")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "remote feature")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	result, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			ProjectRemote:       "upstream",
			Branch:              "pr-explicit-remote",
			Path:                filepath.Join(t.TempDir(), "wt"),
			Number:              36,
			HeadBranch:          "remote-feature",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
		})

	require.NoError(err)
	assert.Equal(headSHA, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
	assert.Equal("upstream",
		worktreeConfig(t, result.Path, "branch.pr-explicit-remote.remote"))
	assert.Equal("upstream",
		worktreeConfig(t, result.Path, "branch.pr-explicit-remote.pushRemote"))
}

func TestCreateWorktreeFromMergeRequestMatchesEquivalentLocalRepositories(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		headURL func(*testing.T, string) string
	}{
		{
			name: "symlink",
			headURL: func(t *testing.T, origin string) string {
				t.Helper()
				alias := filepath.Join(t.TempDir(), "origin-alias")
				if err := os.Symlink(origin, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				return alias
			},
		},
		{
			name: "file URL",
			headURL: func(_ *testing.T, origin string) string {
				path := filepath.ToSlash(origin)
				if filepath.VolumeName(origin) != "" &&
					!strings.HasPrefix(path, "/") {
					path = "/" + path
				}
				return (&url.URL{Scheme: "file", Path: path}).String()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, origin, "checkout", "-q", "-b", "alternate")
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "alternate")
			headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
			lifecycleGit(t, origin, "checkout", "-q", "main")

			result, err := CreateWorktreeFromMergeRequest(
				t.Context(), MergeRequestWorktreeOptions{
					Runner:              lifecycleTestRunner(t),
					ProjectRoot:         clone,
					Branch:              "pr-alternate",
					Path:                filepath.Join(t.TempDir(), "worktree"),
					Number:              19,
					HeadBranch:          "alternate",
					HeadRepoCloneURL:    test.headURL(t, origin),
					ProjectRepoIdentity: origin,
				})

			require.NoError(err)
			assert.Equal(headSHA, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
			assert.Equal("origin",
				worktreeConfig(t, result.Path, "branch.pr-alternate.remote"))
		})
	}
}

func TestCreateWorktreeFromMergeRequestCanonicalizesRelativeProjectIdentity(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "relative-project")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "relative project")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")
	relativeOrigin, err := filepath.Rel(clone, origin)
	require.NoError(err)
	require.True(strings.HasPrefix(relativeOrigin, ".."))

	result, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-relative-project",
			Path:                filepath.Join(t.TempDir(), "worktree"),
			Number:              20,
			HeadBranch:          "relative-project",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: relativeOrigin,
		})

	require.NoError(err)
	assert.Equal(headSHA, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
	assert.Equal("origin",
		worktreeConfig(t, result.Path, "branch.pr-relative-project.remote"))
}

func TestCreateWorktreeFromMergeRequestGitLabRef(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "mr-work")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "mr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "mr-work")
	lifecycleGit(t, origin, "update-ref", "refs/merge-requests/5/head", headSHA)
	lifecycleGit(t, origin, "checkout", "-q", "main")

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:      lifecycleTestRunner(t),
			ProjectRoot: clone,
			Branch:      "mr-5",
			Path:        dest,
			Number:      5,
			HeadBranch:  "mr-work",
			Platform:    "gitlab",
		})
	require.NoError(err)
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"))
}

// TestCreateWorktreeFromMergeRequestPullRefFallback covers the no-fork-URL
// scenario: the merge request head is fetched via the platform pull ref and
// no upstream tracking is configured.
func TestCreateWorktreeFromMergeRequestPullRefFallback(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "contributor-work")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "contributor-work")
	lifecycleGit(t, origin, "update-ref", "refs/pull/7/head", headSHA)
	lifecycleGit(t, origin, "checkout", "-q", "main")
	legitimateTrackingSHA := lifecycleGit(t, clone, "rev-parse", "origin/main")
	lifecycleGit(t, clone, "update-ref",
		"refs/remotes/origin/pull/7/head", legitimateTrackingSHA)

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:      lifecycleTestRunner(t),
			ProjectRoot: clone,
			Branch:      "pr-7",
			Path:        dest,
			Number:      7,
			HeadBranch:  "contributor-work",
		})
	require.NoError(err)
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"),
		"worktree starts at the pull ref head")
	assert.Empty(worktreeConfig(t, dest, "branch.pr-7.remote"),
		"no tracking without a fork clone URL")
	assert.Equal(legitimateTrackingSHA, lifecycleGit(
		t, clone, "rev-parse", "refs/remotes/origin/pull/7/head",
	), "pull-ref import preserves legitimate remote-tracking branches")
	assert.Empty(lifecycleGit(
		t, clone, "for-each-ref", "--format=%(refname)",
		"refs/kit/merge-requests/",
	), "temporary managed fetch refs are removed after resolving the head")
}

func TestCreateWorktreeFromMergeRequestRejectsLeftoverBranch(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, clone, "branch", "pr-leftover")
	dest := filepath.Join(t.TempDir(), "wt")

	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-leftover",
			Path:                dest,
			Number:              35,
			HeadBranch:          "contributor-work",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
		})

	require.ErrorIs(err, ErrBranchAlreadyExists)
	require.NotErrorIs(err, ErrWorktreeDestinationExists)
	require.NoDirExists(dest)
}

func TestCreateWorktreeFromMergeRequestPreservesCancellation(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"fetch", "resolve"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, origin, "checkout", "-q", "-b", "cancel-import")
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "cancel")
			lifecycleGit(t, origin, "checkout", "-q", "main")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fetched := false

			_, err := CreateWorktreeFromMergeRequest(
				ctx, MergeRequestWorktreeOptions{
					Runner:              lifecycleTestRunner(t),
					ProjectRoot:         clone,
					Branch:              "pr-cancel-" + phase,
					Path:                filepath.Join(t.TempDir(), "wt"),
					Number:              35,
					HeadBranch:          "cancel-import",
					HeadRepoCloneURL:    origin,
					ProjectRepoIdentity: identityOfCloneURL(origin),
					RunGit: func(
						runCtx context.Context, runner gitcmd.Runner,
						dir string, args ...string,
					) ([]byte, error) {
						if len(args) > 0 && args[0] == "fetch" {
							if phase == "fetch" {
								cancel()
								return nil, errors.New("process killed")
							}
							fetched = true
						}
						if phase == "resolve" && fetched &&
							len(args) > 0 && args[0] == "rev-parse" {
							cancel()
							return nil, errors.New("process killed")
						}
						stdout, stderr, runErr := runner.Run(
							runCtx, dir, nil, args...,
						)
						return append(stdout, stderr...), runErr
					},
				})

			require.ErrorIs(err, context.Canceled)
			var changeErr *ChangeRequestError
			assert.NotErrorAs(err, &changeErr)
		})
	}
}

func TestCreateWorktreeFromMergeRequestRejectsChangedHead(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "changed-head")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "changed-head")
	lifecycleGit(t, origin, "update-ref", "refs/pull/8/head", headSHA)
	lifecycleGit(t, origin, "checkout", "-q", "main")

	dest := filepath.Join(t.TempDir(), "wt")
	result, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:          lifecycleTestRunner(t),
			ProjectRoot:     clone,
			Branch:          "pr-8",
			Path:            dest,
			Number:          8,
			HeadBranch:      "changed-head",
			ExpectedHeadSHA: lifecycleGit(t, origin, "rev-parse", "main"),
		})

	assert.Empty(result.Path)
	var changeErr *ChangeRequestError
	require.ErrorAs(err, &changeErr)
	assert.Equal(ChangeRequestHeadChanged, changeErr.Kind)
	assert.NoDirExists(dest)
}

func TestCreateWorktreeFromMergeRequestRejectsIncompatibleCommonConfig(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "core worktree", key: "core.worktree", value: "/other/worktree"},
		{name: "bare repository", key: "core.bare", value: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, origin, "checkout", "-q", "-b", "config-check")
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "request head")
			headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
			lifecycleGit(t, origin, "checkout", "-q", "main")
			lifecycleGit(t, clone, "config", "--local", test.key, test.value)

			dest := filepath.Join(t.TempDir(), "wt")
			_, err := CreateWorktreeFromMergeRequest(
				t.Context(), MergeRequestWorktreeOptions{
					Runner:      lifecycleTestRunner(t),
					ProjectRoot: clone, Branch: "pr-config-check", Path: dest,
					Number: 26, HeadBranch: "config-check",
					HeadRepoCloneURL:    origin,
					ProjectRepoIdentity: identityOfCloneURL(origin),
					ExpectedHeadSHA:     headSHA,
				})

			require.Error(err)
			require.ErrorContains(err, test.key)
			assert.NoDirExists(dest)
		})
	}
}

func TestCreateWorktreeFromMergeRequestIsolatesUntrustedTreeGitPrograms(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("executable Git hook and filter fixture requires POSIX")
	}
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	markers := t.TempDir()
	hookMarker := filepath.Join(markers, "hook-ran")
	filterMarker := filepath.Join(markers, "filter-ran")
	fsmonitorMarker := filepath.Join(markers, "fsmonitor-ran")
	dest := filepath.Join(t.TempDir(), "wt")

	lifecycleGit(t, origin, "checkout", "-q", "-b", "untrusted-tree")
	require.NoError(os.MkdirAll(filepath.Join(origin, ".githooks"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".githooks", "post-checkout"),
		[]byte("#!/bin/sh\n: > "+hookMarker+"\n"), 0o755,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".gitattributes"),
		[]byte(
			"payload filter=owned diff=owned merge=owned\n"+
				"added diff=owned\n",
		), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), []byte("external"), 0o644,
	))
	lifecycleGit(t, origin, "add", ".githooks", ".gitattributes", "payload")
	lifecycleGit(t, origin, "commit", "-qm", "untrusted tree programs")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	filterScript := filepath.Join(markers, "filter.sh")
	require.NoError(os.WriteFile(
		filterScript,
		[]byte("#!/bin/sh\n: > "+filterMarker+"\ncat\n"), 0o755,
	))
	fsmonitorScript := filepath.Join(markers, "fsmonitor.sh")
	require.NoError(os.WriteFile(
		fsmonitorScript,
		[]byte("#!/bin/sh\n: > "+fsmonitorMarker+"\nprintf '0\\n'\n"), 0o755,
	))
	lifecycleGit(t, clone, "config", "core.hooksPath",
		filepath.Join(dest, ".githooks"))
	lifecycleGit(t, clone, "config", "core.fsmonitor", fsmonitorScript)
	lifecycleGit(t, clone, "config", "filter.owned.smudge", filterScript)
	lifecycleGit(t, clone, "config", "filter.owned.clean", filterScript)
	lifecycleGit(t, clone, "config", "filter.owned.required", "true")
	lifecycleGit(t, clone, "config", "diff.owned.command", filterScript)
	lifecycleGit(t, clone, "config", "diff.owned.textconv", filterScript)
	lifecycleGit(t, clone, "config", "merge.owned.driver", filterScript)

	result, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-untrusted",
			Path:                dest,
			Number:              21,
			HeadBranch:          "untrusted-tree",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA:     headSHA,
		})

	require.NoError(err)
	assert.Equal(dest, result.Path)
	assert.NoFileExists(hookMarker)
	assert.NoFileExists(filterMarker)
	assert.Equal("external", lifecycleGit(t, dest, "show", "HEAD:payload"))
	assert.Equal("false", worktreeConfig(t, dest, "core.fsmonitor"))
	assert.Empty(worktreeConfig(t, dest, "filter.owned.clean"))
	assert.Empty(worktreeConfig(t, dest, "filter.owned.smudge"))
	assert.Empty(worktreeConfig(t, dest, "filter.owned.process"))
	assert.Equal("false", worktreeConfig(t, dest, "filter.owned.required"))
	assert.Equal(safeExternalDiffCommand,
		worktreeConfig(t, dest, "diff.owned.command"))
	assert.Equal(safeTextconvCommand,
		worktreeConfig(t, dest, "diff.owned.textconv"))
	assert.Equal(expectedSafeMergeDriverCommand(t, dest),
		worktreeConfig(t, dest, "merge.owned.driver"))

	if err := os.Remove(fsmonitorMarker); err != nil {
		require.ErrorIs(err, os.ErrNotExist)
	}
	dirty, dirtyErr := WorktreeIsDirty(t.Context(), dest)
	require.NoError(dirtyErr)
	assert.False(dirty)
	assert.NoFileExists(fsmonitorMarker,
		"persistent isolation protects later ordinary Git commands")

	require.NoError(os.WriteFile(
		filepath.Join(dest, "payload"), []byte("changed"), 0o644,
	))
	diff := lifecycleGit(t, dest, "diff", "--", "payload")
	assert.Contains(diff, "-external")
	assert.Contains(diff, "+changed")
	assert.Contains(diff, `\ No newline at end of file`)
	require.NoError(os.WriteFile(
		filepath.Join(dest, "added"), []byte("new"), 0o644,
	))
	lifecycleGit(t, dest, "add", "-N", "added")
	addedDiff := lifecycleGit(t, dest, "diff", "--", "added")
	assert.Contains(addedDiff, "+new")
	assert.Contains(addedDiff, `\ No newline at end of file`)
	lifecycleGit(t, dest, "add", "payload")
	assert.NoFileExists(filterMarker,
		"later diff and clean operations keep attribute programs disabled")
}

func TestCreateWorktreeFromMergeRequestDisablesLaterSubmoduleFetches(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)

	moduleOrigin := initLifecycleRepo(t)
	require.NoError(os.WriteFile(
		filepath.Join(moduleOrigin, "tracked.txt"), []byte("original\n"), 0o644,
	))
	lifecycleGit(t, moduleOrigin, "add", "tracked.txt")
	lifecycleGit(t, moduleOrigin, "commit", "-qm", "module content")

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "submodule-tree")
	lifecycleGit(t, origin,
		"-c", "protocol.file.allow=always",
		"submodule", "add", "-q", moduleOrigin, "deps/module",
	)
	lifecycleGit(t, origin, "commit", "-qam", "add recursive submodule")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")
	lifecycleGit(t, clone, "config", "fetch.recurseSubmodules", "true")
	lifecycleGit(t, clone, "config",
		"submodule.deps/module.fetchRecurseSubmodules", "true")

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:      lifecycleTestRunner(t),
			ProjectRoot: clone, Branch: "pr-submodule-fetch", Path: dest,
			Number: 29, HeadBranch: "submodule-tree",
			HeadRepoCloneURL: origin, ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA: headSHA,
		})
	require.NoError(err)
	assert.Equal("false", worktreeOnlyConfig(
		t, dest, "fetch.recurseSubmodules",
	))
	assert.Equal("false", worktreeOnlyConfig(
		t, dest, "submodule.deps/module.fetchRecurseSubmodules",
	))

	lifecycleGit(t, dest,
		"-c", "protocol.file.allow=always",
		"submodule", "update", "--init", "--", "deps/module",
	)
	module := filepath.Join(dest, "deps", "module")
	before := lifecycleGit(t, module, "rev-parse", "refs/remotes/origin/main")
	require.NoError(os.WriteFile(
		filepath.Join(moduleOrigin, "tracked.txt"), []byte("advanced\n"), 0o644,
	))
	lifecycleGit(t, moduleOrigin, "commit", "-qam", "advance module")
	advanced := lifecycleGit(t, moduleOrigin, "rev-parse", "HEAD")
	require.NotEqual(before, advanced)

	lifecycleGit(t, dest,
		"-c", "protocol.file.allow=always",
		"fetch", "origin",
	)
	assert.Equal(before,
		lifecycleGit(t, module, "rev-parse", "refs/remotes/origin/main"),
		"later project fetch does not recurse into the imported submodule",
	)
}

func TestCreateWorktreeFromMergeRequestNeutralizesCaseDistinctAttributeDrivers(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "driver-selection")
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".gitattributes"),
		[]byte(
			"upper filter=Safe diff=Safe merge=Safe\n"+
				"lower filter=safe diff=safe merge=safe\n",
		), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "upper"), []byte("upper\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "lower"), []byte("lower\n"), 0o644,
	))
	lifecycleGit(t, origin, "add", ".gitattributes", "upper", "lower")
	lifecycleGit(t, origin, "commit", "-qm", "select configured drivers")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	for _, driver := range []string{"Safe", "safe"} {
		lifecycleGit(t, clone, "config", "filter."+driver+".smudge", "false")
		lifecycleGit(t, clone, "config", "filter."+driver+".required", "true")
		lifecycleGit(t, clone, "config", "diff."+driver+".command", "false")
		lifecycleGit(t, clone, "config", "merge."+driver+".driver", "false")
	}

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-drivers",
			Path:                dest,
			Number:              22,
			HeadBranch:          "driver-selection",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA:     headSHA,
		})

	require.NoError(err)
	for _, driver := range []string{"Safe", "safe"} {
		assert.Equal("false",
			worktreeOnlyConfig(t, dest, "filter."+driver+".required"))
		assert.Equal(safeExternalDiffCommand,
			worktreeOnlyConfig(t, dest, "diff."+driver+".command"))
		assert.Equal(expectedSafeMergeDriverCommand(t, dest),
			worktreeOnlyConfig(t, dest, "merge."+driver+".driver"))
	}
}

func TestCreateWorktreeFromMergeRequestDoesNotPATHSearchDriverHelpers(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("executable PATH fixture requires POSIX")
	}
	require := require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	marker := filepath.Join(t.TempDir(), "attacker-sh-ran")
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".gitattributes"),
		[]byte("payload diff=owned merge=owned\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), []byte("base\n"), 0o644,
	))
	for _, helper := range []string{"git", "sh"} {
		require.NoError(os.WriteFile(
			filepath.Join(origin, helper),
			[]byte("#!/bin/sh\n: > \""+marker+"\"\nexit 0\n"), 0o755,
		))
	}
	lifecycleGit(t, origin, "add", ".gitattributes", "payload", "git", "sh")
	lifecycleGit(t, origin, "commit", "-qm", "PATH driver base")
	lifecycleGit(t, origin, "checkout", "-q", "-b", "path-driver")
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), []byte("current\n"), 0o644,
	))
	lifecycleGit(t, origin, "commit", "-qam", "PATH driver current")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")
	lifecycleGit(t, origin, "checkout", "-q", "-b", "path-driver-other")
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), []byte("other\n"), 0o644,
	))
	lifecycleGit(t, origin, "commit", "-qam", "PATH driver other")
	lifecycleGit(t, origin, "checkout", "-q", "main")
	lifecycleGit(t, clone, "fetch", "-q", "origin",
		"refs/heads/path-driver-other:refs/remotes/origin/path-driver-other")
	lifecycleGit(t, clone, "config", "diff.owned.command", "false")
	lifecycleGit(t, clone, "config", "merge.owned.driver", "false")

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:      lifecycleTestRunner(t),
			ProjectRoot: clone, Branch: "pr-path-driver", Path: dest,
			Number: 28, HeadBranch: "path-driver",
			HeadRepoCloneURL: origin, ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA: headSHA,
		})
	require.NoError(err)

	require.NoError(os.WriteFile(
		filepath.Join(dest, "payload"), []byte("changed\n"), 0o644,
	))
	diffCmd := lifecycleGitCommand(t, dest, "diff", "--", "payload")
	diffCmd.Env = append(diffCmd.Env, "PATH="+dest+":"+os.Getenv("PATH"))
	diff, err := diffCmd.CombinedOutput()
	require.NoError(err, string(diff))
	assert.Contains(string(diff), "-current")
	assert.Contains(string(diff), "+changed")
	assert.NoFileExists(marker)
	require.NoError(os.WriteFile(
		filepath.Join(dest, "payload"), []byte("current\n"), 0o644,
	))

	mergeCmd := lifecycleGitCommand(
		t, dest, "merge", "refs/remotes/origin/path-driver-other",
	)
	mergeCmd.Env = append(mergeCmd.Env, "PATH="+dest+":"+os.Getenv("PATH"))
	out, err := mergeCmd.CombinedOutput()

	require.Error(err, string(out))
	assert.Equal("UU payload",
		lifecycleGit(t, dest, "status", "--short", "--", "payload"))
	payload, readErr := os.ReadFile(filepath.Join(dest, "payload"))
	require.NoError(readErr)
	assert.Equal(
		"<<<<<<< current\ncurrent\n||||||| base\nbase\n=======\nother\n>>>>>>> other\n",
		string(payload),
	)
	assert.NoFileExists(marker)
}

func TestMergeRequestRollbackRetainsIsolatedRunner(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("executable fsmonitor and filter fixtures require POSIX")
	}
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "rollback-isolation")
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".gitattributes"),
		[]byte("payload filter=rollback\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), []byte("external\n"), 0o644,
	))
	lifecycleGit(t, origin, "add", ".gitattributes", "payload")
	lifecycleGit(t, origin, "commit", "-qm", "rollback isolation fixture")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	markers := t.TempDir()
	fsmonitorMarker := filepath.Join(markers, "fsmonitor-ran")
	filterMarker := filepath.Join(markers, "filter-ran")
	fsmonitorScript := filepath.Join(markers, "fsmonitor.sh")
	filterScript := filepath.Join(markers, "filter.sh")
	require.NoError(os.WriteFile(
		fsmonitorScript,
		[]byte("#!/bin/sh\n: > \""+fsmonitorMarker+"\"\nprintf '0\\n'\n"), 0o755,
	))
	require.NoError(os.WriteFile(
		filterScript,
		[]byte("#!/bin/sh\n: > \""+filterMarker+"\"\ncat\n"), 0o755,
	))
	globalConfig := filepath.Join(markers, "global.gitconfig")
	require.NoError(os.WriteFile(globalConfig, []byte(
		"[core]\n\tfsmonitor = "+fsmonitorScript+"\n"+
			"[filter \"rollback\"]\n"+
			"\tclean = "+filterScript+"\n"+
			"\tsmudge = "+filterScript+"\n"+
			"\trequired = true\n",
	), 0o600))
	runner := gitcmd.New()
	runner.NullGlobalConfig = false
	// StripEnv would discard the GIT_CONFIG_GLOBAL override below and let git
	// fall back to the host's real global config; Env is already sanitized.
	runner.StripEnv = false
	runner.Env = append(
		isolatedLifecycleBaseEnv(t),
		"GIT_CONFIG_GLOBAL="+globalConfig,
	)

	result, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			ProjectRoot: clone, Branch: "pr-rollback-isolation",
			Path:   filepath.Join(t.TempDir(), "wt"),
			Number: 27, HeadBranch: "rollback-isolation",
			HeadRepoCloneURL: origin, ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA: headSHA, Runner: runner,
		})
	require.NoError(err)
	for _, marker := range []string{fsmonitorMarker, filterMarker} {
		if removeErr := os.Remove(marker); removeErr != nil {
			require.ErrorIs(removeErr, os.ErrNotExist)
		}
	}

	remaining, err := result.Rollback(t.Context())

	require.NoError(err)
	assert.Empty(remaining.Path)
	assert.Empty(remaining.Branch)
	assert.NoFileExists(fsmonitorMarker)
	assert.NoFileExists(filterMarker)
}

func TestCreateWorktreeFromMergeRequestInspectsSelectedConfigFiles(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "selected-config")
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".gitattributes"),
		[]byte(
			"payload filter=checkout diff=selected merge=selected\n"+
				"selected-payload filter=selected\n",
		), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), []byte("external\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "selected-payload"), []byte("selected\n"), 0o644,
	))
	lifecycleGit(t, origin, "add", ".gitattributes", "payload", "selected-payload")
	lifecycleGit(t, origin, "commit", "-qm", "select alternate config driver")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	configDir := t.TempDir()
	globalConfig := filepath.Join(configDir, "global.gitconfig")
	systemConfig := filepath.Join(configDir, "system.gitconfig")
	require.NoError(os.WriteFile(filepath.Join(configDir, ".gitconfig"), []byte(
		"[filter \"checkout\"]\n"+
			"\tsmudge = false\n"+
			"\trequired = true\n",
	), 0o600))
	require.NoError(os.WriteFile(globalConfig, []byte(
		"[filter \"selected\"]\n"+
			"\tsmudge = false\n"+
			"\trequired = true\n"+
			"[diff \"selected\"]\n"+
			"\tcommand = false\n",
	), 0o600))
	require.NoError(os.WriteFile(systemConfig, []byte(
		"[merge \"selected\"]\n\tdriver = false\n",
	), 0o600))
	runner := gitcmd.New()
	runner.NullGlobalConfig = false
	runner.NoSystemConfig = false
	runner.Env = append(isolatedLifecycleBaseEnv(t),
		"HOME="+configDir,
		"GIT_CONFIG_GLOBAL="+globalConfig,
		"GIT_CONFIG_SYSTEM="+systemConfig,
	)

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			ProjectRoot: clone, Branch: "pr-selected-config", Path: dest,
			Number: 23, HeadBranch: "selected-config",
			HeadRepoCloneURL: origin, ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA: headSHA, Runner: runner,
		})

	require.NoError(err)
	assert.Equal("false",
		worktreeOnlyConfig(t, dest, "filter.checkout.required"))
	assert.Equal("false",
		worktreeOnlyConfig(t, dest, "filter.selected.required"))
	assert.Equal(safeExternalDiffCommand,
		worktreeOnlyConfig(t, dest, "diff.selected.command"))
	assert.Equal(expectedSafeMergeDriverCommand(t, dest),
		worktreeOnlyConfig(t, dest, "merge.selected.driver"))
}

func TestCreateWorktreeFromMergeRequestRejectsInheritedCommandScopeConfig(
	t *testing.T,
) {
	for _, test := range []struct {
		name           string
		runner         func(*testing.T, string) gitcmd.Runner
		wantError      string
		preserveMarker bool
	}{
		{
			name: "GIT_CONFIG_COUNT",
			runner: func(t *testing.T, marker string) gitcmd.Runner {
				t.Helper()
				runner := gitcmd.New()
				runner.Env = append(isolatedLifecycleBaseEnv(t),
					"GIT_CONFIG_COUNT=1",
					"GIT_CONFIG_KEY_0=filter.inherited.smudge",
					"GIT_CONFIG_VALUE_0=false",
				)
				return runner
			},
		},
		{
			name: "GIT_CONFIG_PARAMETERS",
			runner: func(t *testing.T, marker string) gitcmd.Runner {
				t.Helper()
				runner := gitcmd.New()
				runner.Env = append(isolatedLifecycleBaseEnv(t),
					"GIT_CONFIG_PARAMETERS='filter.inherited.smudge'='false'",
				)
				return runner
			},
		},
		{
			name: "Runner Config",
			runner: func(t *testing.T, marker string) gitcmd.Runner {
				t.Helper()
				return gitcmd.Runner{
					Env:      isolatedLifecycleBaseEnv(t),
					StripEnv: true,
					Config: []gitcmd.Config{{
						Key: "filter.inherited.smudge", Value: "false",
					}},
				}
			},
		},
		{
			name: "absolute include",
			runner: func(t *testing.T, marker string) gitcmd.Runner {
				t.Helper()
				config := filepath.Join(t.TempDir(), "included.gitconfig")
				require.NoError(t, os.WriteFile(
					config,
					[]byte("[filter \"inherited\"]\n\tsmudge = false\n"),
					0o600,
				))
				runner := gitcmd.New()
				runner.Env = append(isolatedLifecycleBaseEnv(t),
					"GIT_CONFIG_COUNT=1",
					"GIT_CONFIG_KEY_0=include.path",
					"GIT_CONFIG_VALUE_0="+config,
				)
				return runner
			},
		},
		{
			name: "conditional include",
			runner: func(t *testing.T, marker string) gitcmd.Runner {
				t.Helper()
				config := filepath.Join(t.TempDir(), "conditional.gitconfig")
				require.NoError(t, os.WriteFile(config, []byte("[filter \"conditional\"]\n\tsmudge = false\n"), 0o600))
				runner := gitcmd.New()
				runner.Env = append(isolatedLifecycleBaseEnv(t),
					"GIT_CONFIG_COUNT=1",
					"GIT_CONFIG_KEY_0=includeIf.onbranch:pr-inherited-config.path",
					"GIT_CONFIG_VALUE_0="+config,
				)
				return runner
			},
		},
		{
			name: "worktree override",
			runner: func(t *testing.T, marker string) gitcmd.Runner {
				t.Helper()
				require.NoError(t, os.WriteFile(marker, []byte("preserve"), 0o600))
				return lifecycleTestRunner(t).WithConfig("core.worktree", filepath.Dir(marker))
			},
			preserveMarker: true,
		},
		{
			name: "relative include",
			runner: func(t *testing.T, marker string) gitcmd.Runner {
				t.Helper()
				runner := gitcmd.New()
				runner.Env = append(isolatedLifecycleBaseEnv(t),
					"GIT_CONFIG_COUNT=1",
					"GIT_CONFIG_KEY_0=include.path",
					"GIT_CONFIG_VALUE_0=.gitconfig",
				)
				return runner
			},
			wantError: "relative config includes must come from files",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, origin, "checkout", "-q", "-b", "inherited-config")
			require.NoError(os.WriteFile(
				filepath.Join(origin, ".gitattributes"),
				[]byte("payload filter=inherited\n"), 0o644,
			))
			require.NoError(os.WriteFile(
				filepath.Join(origin, "payload"), []byte("external\n"), 0o644,
			))
			if test.name == "relative include" {
				require.NoError(os.WriteFile(filepath.Join(origin, ".gitconfig"), []byte("[diff \"materialized\"]\n\tcommand = false\n"), 0o644))
			}
			lifecycleGit(t, origin, "add", ".")
			lifecycleGit(t, origin, "commit", "-qm", "select inherited driver")
			headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
			lifecycleGit(t, origin, "checkout", "-q", "main")

			marker := filepath.Join(t.TempDir(), "keep")
			dest := filepath.Join(t.TempDir(), "wt")
			_, err := CreateWorktreeFromMergeRequest(
				t.Context(), MergeRequestWorktreeOptions{
					ProjectRoot: clone, Branch: "pr-inherited-config", Path: dest,
					Number: 30, HeadBranch: "inherited-config",
					HeadRepoCloneURL:    origin,
					ProjectRepoIdentity: identityOfCloneURL(origin),
					ExpectedHeadSHA:     headSHA, Runner: test.runner(t, marker),
				})

			require.Error(err)
			wantError := test.wantError
			if wantError == "" {
				wantError = "command-scope Git configuration cannot be isolated"
			}
			require.ErrorContains(err, wantError)
			if test.preserveMarker {
				assert.FileExists(marker)
			}
			if test.name == "conditional include" {
				assert.False(branchExistsInRepo(t, clone, "pr-inherited-config"))
			}
			assert.NoDirExists(dest)
		})
	}
}

func TestCreateWorktreeFromMergeRequestDisablesConfiguredHooks(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "configured-hook")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "configured hook")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(t.TempDir(), "configured-hook")
	require.NoError(os.WriteFile(hook, fmt.Appendf(nil, "#!/bin/sh\nprintf ran > %q\n", filepath.ToSlash(marker)), 0o755))
	lifecycleGit(t, clone, "config", "hook.import.command", filepath.ToSlash(hook))
	for _, event := range []string{"post-checkout", "post-index-change", "reference-transaction"} {
		lifecycleGit(t, clone, "config", "--add", "hook.import.event", event)
	}
	conditional := filepath.Join(t.TempDir(), "conditional.config")
	lifecycleGit(t, clone, "config", "--file", conditional, "hook.branch-specific.command", filepath.ToSlash(hook))
	lifecycleGit(t, clone, "config", "--file", conditional, "hook.branch-specific.event", "post-index-change")
	lifecycleGit(t, clone, "config", "includeIf.onbranch:pr-configured-hook.path", filepath.ToSlash(conditional))
	// Verify the installed Git actually executes this fixture where supported.
	_, listErr := lifecycleTestRunner(t).Output(t.Context(), clone, "hook", "list", "post-checkout")
	if listErr == nil {
		lifecycleGit(t, clone, "hook", "run", "post-checkout")
		require.FileExists(marker)
		require.NoError(os.Remove(marker))
	}
	dest := filepath.Join(t.TempDir(), "wt")
	created, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
		Runner: lifecycleTestRunner(t), ProjectRoot: clone, Branch: "pr-configured-hook", Path: dest,
		Number: 34, HeadBranch: "configured-hook", HeadRepoCloneURL: origin,
		ProjectRepoIdentity: identityOfCloneURL(origin), ExpectedHeadSHA: headSHA,
	})
	require.NoError(err)
	require.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"))
	require.NoFileExists(marker)
	// Persisted isolation also covers later native commands in the checkout.
	lifecycleGit(t, dest, "reset", "--hard", "HEAD")
	require.NoFileExists(marker)
	require.Equal("false", lifecycleGit(t, dest, "config", "--get", "hook.import.enabled"))
	require.Equal("false", lifecycleGit(t, dest, "config", "--get", "hook.branch-specific.enabled"))
	require.Equal(filepath.ToSlash(hook), lifecycleGit(t, clone, "config", "--get", "hook.import.command"))
	_, err = created.Rollback(t.Context(), RollbackFreshOwned)
	require.NoError(err)
	require.NoFileExists(marker)
	if listErr == nil {
		lifecycleGit(t, clone, "hook", "run", "post-checkout")
		require.FileExists(marker)
	}
}

func TestIsolationSensitiveConfigKeyIncludesConfiguredHooks(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"hook.import.command",
		"hook.import.event",
		"hook.import.enabled",
	} {
		assert.True(t, isolationSensitiveConfigKey(key), key)
	}
}

func TestPathWithinRootByIdentityRecognizesFilesystemAliases(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := filepath.Join(root, "config")
	require.NoError(t, os.WriteFile(child, []byte("[safe]\n"), 0o600))
	assert.True(t, pathWithinRootByIdentity(root, child))

	caseAlias := strings.ToUpper(child)
	if _, err := os.Stat(caseAlias); err != nil {
		t.Skip("temporary filesystem is case-sensitive")
	}
	assert.True(t, pathWithinRootByIdentity(root, caseAlias))
}

func TestSectionLevelConfigKeysDoNotNameSubsections(t *testing.T) {
	t.Parallel()
	assert.Empty(t, configuredGitHooks([]string{
		"hook.command",
		"hook.event",
	}))

	_, ok := configuredSubmoduleName("submodule.path")
	assert.False(t, ok)

	assert.False(t, configuredAttributeDrivers([]string{
		"filter.clean",
		"diff.command",
		"merge.driver",
	}).configured())
}

func TestSafeTextconvPreservesUnterminatedLines(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	require.NoError(os.WriteFile(
		filepath.Join(repo, ".gitattributes"),
		[]byte("payload diff=owned\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(repo, "payload"), []byte("before"), 0o644,
	))
	lifecycleGit(t, repo, "add", ".gitattributes", "payload")
	lifecycleGit(t, repo, "commit", "-qm", "textconv fixture")
	lifecycleGit(t, repo, "config", "diff.owned.textconv", safeTextconvCommand)
	require.NoError(os.WriteFile(
		filepath.Join(repo, "payload"), []byte("after"), 0o644,
	))

	diff := lifecycleGit(t, repo, "diff", "--", "payload")

	assert.Contains(diff, "-before")
	assert.Contains(diff, "+after")
	assert.Contains(diff, `\ No newline at end of file`)
}

func TestCreateWorktreeFromMergeRequestRejectsConfigFromMaterializedTree(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "materialized-config")
	config := []byte("[diff \"materialized\"]\n\tcommand = false\n")
	require.NoError(os.WriteFile(filepath.Join(origin, ".gitconfig"), config, 0o644))
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".gitattributes"),
		[]byte("payload diff=materialized\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), []byte("external\n"), 0o644,
	))
	lifecycleGit(t, origin, "add", ".gitconfig", ".gitattributes", "payload")
	lifecycleGit(t, origin, "commit", "-qm", "materialized config")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	runner := gitcmd.New()
	runner.NullGlobalConfig = false
	// StripEnv would discard the GIT_CONFIG_GLOBAL override below and let git
	// fall back to the host's real global config; Env is already sanitized.
	runner.StripEnv = false
	runner.Env = append(
		isolatedLifecycleBaseEnv(t),
		"GIT_CONFIG_GLOBAL=.gitconfig",
	)
	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			ProjectRoot: clone, Branch: "pr-materialized-config", Path: dest,
			Number: 24, HeadBranch: "materialized-config",
			HeadRepoCloneURL: origin, ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA: headSHA, Runner: runner,
		})

	require.Error(err)
	require.ErrorContains(err, "configuration inside merge request worktree")
	assert.NoDirExists(dest)
}

func TestMergeRequestRepositoriesAcceptNormalizedHostedIdentity(t *testing.T) {
	t.Parallel()
	assert.True(t, mergeRequestRepositoriesEqual(
		"https://EXAMPLE.COM/Owner/Widget.git",
		"https://example.com/Owner/Widget.git",
	))
	assert.False(t, mergeRequestRepositoriesEqual(
		"https://example.com/Owner/Widget.git",
		"https://example.com/owner/widget.git",
	))
	for _, rawURL := range []string{
		"https://example.com:8443/Owner/Widget.git",
		"ssh://[2001:db8::1]:2222/Owner/Widget.git",
	} {
		identity := CloneURLIdentity(rawURL)
		assert.NotEqual(t, rawURL, identity)
		assert.True(t, mergeRequestRepositoriesEqual(rawURL, identity),
			"%s should match normalized identity %s", rawURL, identity)
	}
}

func TestCreateWorktreeFromMergeRequestInspectsConditionalIncludes(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "conditional-config")
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".gitattributes"),
		[]byte(
			"branch-payload filter=branch-driver\n"+
				"gitdir-payload filter=gitdir-driver\n",
		), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "branch-payload"), []byte("branch\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "gitdir-payload"), []byte("gitdir\n"), 0o644,
	))
	lifecycleGit(t, origin, "add", ".")
	lifecycleGit(t, origin, "commit", "-qm", "select conditional drivers")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	includes := t.TempDir()
	branchInclude := filepath.Join(includes, "branch.gitconfig")
	gitdirInclude := filepath.Join(includes, "gitdir.gitconfig")
	require.NoError(os.WriteFile(branchInclude, []byte(
		"[filter \"branch-driver\"]\n\tsmudge = false\n\trequired = true\n",
	), 0o600))
	require.NoError(os.WriteFile(gitdirInclude, []byte(
		"[filter \"gitdir-driver\"]\n\tsmudge = false\n\trequired = true\n",
	), 0o600))
	lifecycleGit(t, clone, "config", "--add",
		"includeIf.onbranch:pr-conditional.path", branchInclude)
	lifecycleGit(t, clone, "config", "--add",
		"includeIf.gitdir:./worktrees/**.path", gitdirInclude)

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:      lifecycleTestRunner(t),
			ProjectRoot: clone, Branch: "pr-conditional", Path: dest,
			Number: 24, HeadBranch: "conditional-config",
			HeadRepoCloneURL: origin, ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA: headSHA,
		})

	require.NoError(err)
	assert.Equal("false",
		worktreeOnlyConfig(t, dest, "filter.branch-driver.required"))
	assert.Equal("false",
		worktreeOnlyConfig(t, dest, "filter.gitdir-driver.required"))
}

func TestCreateWorktreeFromMergeRequestReportsCleanupFailure(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "cleanup-failure")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "request head")
	headSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	runGit := func(
		ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
	) ([]byte, error) {
		for index, arg := range args {
			if arg == "reset" && index+1 < len(args) &&
				args[index+1] == "--hard" {
				return nil, errors.New("materialization failed")
			}
		}
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			return nil, errors.New("cleanup failed")
		}
		stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
		return append(stdout, stderr...), err
	}

	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:      lifecycleTestRunner(t),
			ProjectRoot: clone, Branch: "pr-cleanup-failure",
			Path:   filepath.Join(t.TempDir(), "wt"),
			Number: 25, HeadBranch: "cleanup-failure",
			HeadRepoCloneURL: origin, ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA: headSHA, RunGit: runGit,
		})

	require.Error(err)
	require.ErrorContains(err, "materialization failed")
	assert.ErrorContains(err, "cleanup failed")
}

// TestCreateWorktreeFromMergeRequestFork covers the fork scenario: checkout
// still comes from origin's pull ref, while tracking is configured against
// a dedicated fork remote.
func TestCreateWorktreeFromMergeRequestFork(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)

	// The fork: a second clone of origin with the head branch.
	fork := filepath.Join(t.TempDir(), "fork")
	lifecycleGit(t, filepath.Dir(origin), "clone", "-q", origin, fork)
	lifecycleGit(t, fork, "config", "user.email", "t@e.st")
	lifecycleGit(t, fork, "config", "user.name", "Tester")
	lifecycleGit(t, fork, "checkout", "-q", "-b", "fork-work")
	lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "fork pr work")
	lifecycleGit(t, fork, "tag", "contributor-tag")
	headSHA := lifecycleGit(t, fork, "rev-parse", "fork-work")

	// GitHub exposes the fork's head on origin's pull ref.
	lifecycleGit(t, origin, "fetch", "-q", fork,
		"+refs/heads/fork-work:refs/pull/9/head")
	lifecycleGit(t, clone, "config", "fetch.recurseSubmodules", "true")
	const fetchHeadSentinel = "preserve existing fetch state\n"
	require.NoError(os.WriteFile(
		filepath.Join(clone, ".git", "FETCH_HEAD"),
		[]byte(fetchHeadSentinel), 0o644,
	))

	dest := filepath.Join(t.TempDir(), "wt")
	var fetches [][]string
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-9",
			Path:                dest,
			Number:              9,
			HeadBranch:          "fork-work",
			HeadRepoCloneURL:    fork,
			ProjectRepoIdentity: identityOfCloneURL(origin),
			RunGit: func(
				ctx context.Context, runner gitcmd.Runner,
				dir string, args ...string,
			) ([]byte, error) {
				if len(args) > 0 && args[0] == "fetch" {
					fetches = append(fetches, append([]string(nil), args...))
				}
				stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
				return append(stdout, stderr...), runErr
			},
		})
	require.NoError(err)
	require.Len(fetches, 2)
	for _, fetch := range fetches {
		assert.Contains(fetch, "--no-tags")
		assert.Contains(fetch, "--no-write-fetch-head")
		assert.Contains(fetch, "--no-recurse-submodules")
	}
	fetchHead, readErr := os.ReadFile(filepath.Join(clone, ".git", "FETCH_HEAD"))
	require.NoError(readErr)
	assert.Equal(fetchHeadSentinel, string(fetchHead))
	tagCommand := lifecycleGitCommand(
		t, clone, "show-ref", "--verify", "--quiet", "refs/tags/contributor-tag",
	)
	require.Error(tagCommand.Run())
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"))
	remote := worktreeConfig(t, dest, "branch.pr-9.remote")
	assert.NotEmpty(remote, "fork tracking remote configured")
	assert.NotEqual("origin", remote)
	assert.Equal("refs/heads/fork-work",
		worktreeConfig(t, dest, "branch.pr-9.merge"))
	remoteURL := lifecycleGit(t, clone, "remote", "get-url", remote)
	assert.Equal(fork, remoteURL)
}

func TestCreateWorktreeFromMergeRequestCanonicalizesRelativeForkURL(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	fork := filepath.Join(filepath.Dir(clone), "fork")
	lifecycleGit(t, filepath.Dir(origin), "clone", "-q", origin, fork)
	lifecycleGit(t, fork, "config", "user.email", "t@e.st")
	lifecycleGit(t, fork, "config", "user.name", "Tester")
	lifecycleGit(t, fork, "checkout", "-q", "-b", "fork-work")
	lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "fork pr work")
	lifecycleGit(t, origin, "fetch", "-q", fork,
		"+refs/heads/fork-work:refs/pull/10/head")

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-10",
			Path:                dest,
			Number:              10,
			HeadBranch:          "fork-work",
			HeadRepoCloneURL:    "../fork",
			ProjectRepoIdentity: identityOfCloneURL(origin),
		})
	require.NoError(err)

	remote := worktreeConfig(t, dest, "branch.pr-10.remote")
	assert.Equal(fork, lifecycleGit(t, clone, "remote", "get-url", remote))
}

func TestEnsureFetchRemoteSkipsPushOnlyNameCollision(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "config",
		"remote.fork.pushurl", filepath.Join(t.TempDir(), "push-only.git"))
	cloneURL := filepath.Join(t.TempDir(), "fork.git")

	name, err := ensureFetchRemote(t.Context(), repo, "fork", cloneURL)

	require.NoError(err)
	assert.Equal("fork-2", name)
	assert.Equal(cloneURL,
		lifecycleGit(t, repo, "config", "--get", "remote.fork-2.url"))
}

func TestEnsureFetchRemoteDoesNotMutateAfterConfigReadFailure(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "remote", "add", "fork",
		filepath.Join(t.TempDir(), "existing.git"))
	ctx := withLifecycleExecution(
		t.Context(), gitcmd.Runner{}, func(
			ctx context.Context, runner gitcmd.Runner,
			dir string, args ...string,
		) ([]byte, error) {
			if strings.Join(args, " ") ==
				"config --get remote.fork.url" {
				return nil, errors.New("config unavailable")
			}
			stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), err
		}, nil,
	)

	_, err := ensureFetchRemote(
		ctx, repo, "fork", filepath.Join(t.TempDir(), "fork.git"),
	)

	require.Error(err)
	require.ErrorContains(err, "inspect remote fork URL")
	assert.Empty(worktreeConfig(t, repo, "remote.fork-2.url"))
}

func TestCanonicalizeMergeRequestCloneURLRejectsEmbeddedSecrets(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	for _, rawURL := range []string{
		"https://user:secret@example.com/acme/widget.git",
		"https://secret@example.com/acme/widget.git",
		"https://example.com/acme/widget.git?access_token=secret",
	} {
		_, err := canonicalizeMergeRequestCloneURL(t.TempDir(), rawURL)

		require.Error(err)
		assert.NotContains(err.Error(), "secret")
		assert.NotContains(err.Error(), rawURL)
	}
}

func TestCanonicalizeMergeRequestCloneURLAllowsSSHUsername(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	const rawURL = "ssh://git@example.com/acme/widget.git"

	cloneURL, err := canonicalizeMergeRequestCloneURL(t.TempDir(), rawURL)

	require.NoError(err)
	assert.Equal(t, rawURL, cloneURL)
}

func TestOptionalTrackingFetchUnavailable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "HTTP forbidden",
			output: "fatal: unable to access 'https://example.com/fork.git/': The requested URL returned error: 403",
			want:   true,
		},
		{
			name:   "HTTP not found",
			output: "fatal: unable to access 'https://example.com/fork.git/': The requested URL returned error: 404",
			want:   true,
		},
		{
			name:   "TLS failure",
			output: "fatal: unable to access 'https://example.com/fork.git/': SSL certificate problem: unable to get local issuer certificate",
			want:   true,
		},
		{
			name:   "connection reset",
			output: "fatal: unable to access 'https://example.com/fork.git/': Recv failure: Connection reset by peer",
			want:   true,
		},
		{
			name:   "local runner failure",
			output: "runner policy failed",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, optionalTrackingFetchUnavailable(
				[]byte(tt.output),
			))
		})
	}
}

// TestCreateWorktreeFromMergeRequestTrackingFetchFailureIsNonFatal: when
// the fork cannot be fetched, the import still succeeds via the pull ref
// and tracking is silently disabled.
func TestCreateWorktreeFromMergeRequestTrackingFetchFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "gone-fork-work")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "gone-fork-work")
	lifecycleGit(t, origin, "update-ref", "refs/pull/11/head", headSHA)
	lifecycleGit(t, origin, "checkout", "-q", "main")

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-11",
			Path:                dest,
			Number:              11,
			HeadBranch:          "gone-fork-work",
			HeadRepoCloneURL:    filepath.Join(t.TempDir(), "no-such-fork"),
			ProjectRepoIdentity: identityOfCloneURL(origin),
		})
	require.NoError(err)
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"))
	assert.Empty(worktreeConfig(t, dest, "branch.pr-11.remote"),
		"tracking silently disabled when the fork fetch fails")
}

func TestCreateWorktreeFromMergeRequestPropagatesTrackingRunnerFailure(
	t *testing.T,
) {
	require := require.New(t)
	origin, clone := initOriginAndClone(t)
	fork := filepath.Join(t.TempDir(), "fork")
	lifecycleGit(t, filepath.Dir(origin), "clone", "-q", origin, fork)
	lifecycleGit(t, fork, "config", "user.email", "t@e.st")
	lifecycleGit(t, fork, "config", "user.name", "Tester")
	lifecycleGit(t, fork, "checkout", "-q", "-b", "tracking-failure")
	lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "fork head")
	headSHA := lifecycleGit(t, fork, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "fetch", "-q", fork,
		"+refs/heads/tracking-failure:refs/pull/38/head")
	runnerErr := errors.New("runner policy failed")
	fetches := 0

	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-tracking-failure",
			Path:                filepath.Join(t.TempDir(), "wt"),
			Number:              38,
			HeadBranch:          "tracking-failure",
			HeadRepoCloneURL:    fork,
			ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA:     headSHA,
			RunGit: func(
				ctx context.Context, runner gitcmd.Runner,
				dir string, args ...string,
			) ([]byte, error) {
				if len(args) > 0 && args[0] == "fetch" {
					fetches++
					if fetches == 2 {
						return nil, runnerErr
					}
				}
				stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
				return append(stdout, stderr...), runErr
			},
		})

	require.ErrorIs(err, runnerErr)
}

// TestCreateWorktreeFromMergeRequestHookFailureRollsBack: a failing setup
// hook rolls back the imported worktree and its branch.
func TestCreateWorktreeFromMergeRequestHookFailureRollsBack(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "feature-x")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	lifecycleGit(t, origin, "checkout", "-q", "main")
	require.NoError(os.WriteFile(
		filepath.Join(clone, "setup.sh"),
		[]byte("#!/bin/sh\nexit 5\n"), 0o755))

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-42",
			Path:                dest,
			Number:              42,
			HeadBranch:          "feature-x",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
			SetupScript:         "setup.sh",
			RunHook:             testHookRunner(),
		})
	var hookErr *HookError
	require.ErrorAs(err, &hookErr)
	assert.Equal(5, hookErr.ExitCode)
	_, statErr := os.Stat(dest)
	assert.True(os.IsNotExist(statErr))
	assert.False(branchExistsInRepo(t, clone, "pr-42"))
}

func TestCreateWorktreeFromMergeRequestRejectsHookFromDestination(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "hook-from-tree")
	require.NoError(os.WriteFile(
		filepath.Join(origin, "setup.sh"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	))
	lifecycleGit(t, origin, "add", "setup.sh")
	lifecycleGit(t, origin, "commit", "-qm", "contributor hook")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	canonicalClone, err := filepath.EvalSymlinks(clone)
	require.NoError(err)
	base := filepath.Join(canonicalClone, ".managed-worktrees")
	require.NoError(os.MkdirAll(base, 0o755))
	dest := filepath.Join(base, "pr-hook")
	hookRan := false
	_, err = CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "pr-hook",
			Path:                dest,
			Number:              31,
			HeadBranch:          "hook-from-tree",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
			SetupScript:         filepath.Join(dest, "setup.sh"),
			RunHook: func(context.Context, HookCommand) error {
				hookRan = true
				return nil
			},
		})

	require.Error(err)
	require.ErrorContains(err, "must already exist")
	assert.False(hookRan)
	assert.NoDirExists(dest)
}
