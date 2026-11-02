package managedworktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/git/internal/shellquote"
)

type mergeDriverFixture struct {
	worktree string
	otherRef string
}

func newMergeDriverFixture(
	t *testing.T, base, current, other []byte,
	currentSubject, otherBranch string,
) mergeDriverFixture {
	t.Helper()
	require := require.New(t)

	if currentSubject == "" {
		currentSubject = "current change"
	}
	if otherBranch == "" {
		otherBranch = "merge-driver-other"
	}

	origin, clone := initOriginAndClone(t)
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".gitattributes"),
		[]byte("payload merge=owned\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), base, 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "binary.dat"), []byte("\x00base\n"), 0o644,
	))
	lifecycleGit(t, origin, "add", ".gitattributes", "payload", "binary.dat")
	lifecycleGit(t, origin, "commit", "-qm", "merge driver base")

	const currentBranch = "merge-driver-current"
	lifecycleGit(t, origin, "checkout", "-q", "-b", currentBranch)
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), current, 0o644,
	))
	lifecycleGit(t, origin, "commit", "-qam", currentSubject)
	currentSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")

	lifecycleGit(t, origin, "checkout", "-q", "main")
	lifecycleGit(t, origin, "checkout", "-q", "-b", otherBranch)
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), other, 0o644,
	))
	lifecycleGit(t, origin, "commit", "-qam", "other change")
	otherSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	otherRef := "refs/remotes/origin/" + otherBranch
	lifecycleGit(t, clone, "fetch", "-q", "origin",
		"refs/heads/"+otherBranch+":"+otherRef)
	require.Equal(otherSHA, lifecycleGit(t, clone, "rev-parse", otherRef))
	lifecycleGit(t, clone, "config", "merge.owned.driver", "false")

	worktree := filepath.Join(t.TempDir(), "worktree")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "imported-current",
			Path:                worktree,
			Number:              112,
			HeadBranch:          currentBranch,
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA:     currentSHA,
		},
	)
	require.NoError(err)

	return mergeDriverFixture{worktree: worktree, otherRef: otherRef}
}

// Keep merge-driver tests serial after Windows label-rebase setup exceeded Git's WaitDelay.
func TestUntrustedTreeMergeDriverMergesNonOverlappingText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("alpha\nmiddle\nomega\n"),
		[]byte("alpha current\nmiddle\nomega\n"),
		[]byte("alpha\nmiddle\nomega other\n"),
		"", "",
	)

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
	out, err := cmd.CombinedOutput()

	require.NoError(err, string(out))
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Contains(string(contents), "alpha current")
	assert.Contains(string(contents), "omega other")
	assert.Empty(lifecycleGit(t, fixture.worktree, "ls-files", "--unmerged"))
}

func TestUntrustedTreeMergeDriverWritesDiff3ConflictMarkers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("before\nbase\nafter\n"),
		[]byte("before\ncurrent\nafter\n"),
		[]byte("before\nother\nafter\n"),
		"", "",
	)

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
	out, err := cmd.CombinedOutput()

	require.Error(err, string(out))
	assert.Equal("UU payload",
		lifecycleGit(t, fixture.worktree, "status", "--short", "--", "payload"))
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Equal(
		"before\n<<<<<<< current\ncurrent\n||||||| base\nbase\n=======\nother\n>>>>>>> other\nafter\n",
		string(contents),
	)
}

func TestUntrustedTreeMergeDriverDoesNotEvaluateGitLabels(t *testing.T) {
	t.Run("merge branch label", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		markers := []string{"branch-dollar-marker", "branch-backtick-marker"}
		fixture := newMergeDriverFixture(t,
			[]byte("base\n"),
			[]byte("current\n"),
			[]byte("other\n"),
			"",
			"other-$(touch${IFS}branch-dollar-marker)-"+
				"`touch${IFS}branch-backtick-marker`",
		)

		cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
		out, err := cmd.CombinedOutput()

		require.Error(err, string(out))
		assert.Equal("UU payload",
			lifecycleGit(t, fixture.worktree, "status", "--short", "--", "payload"))
		for _, marker := range markers {
			assert.NoFileExists(filepath.Join(fixture.worktree, marker))
		}
		contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
		require.NoError(err)
		assert.Equal(
			"<<<<<<< current\ncurrent\n||||||| base\nbase\n=======\nother\n>>>>>>> other\n",
			string(contents),
		)
	})

	t.Run("rebase commit subject label", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		markers := []string{"subject-dollar-marker", "subject-backtick-marker"}
		fixture := newMergeDriverFixture(t,
			[]byte("base\n"),
			[]byte("current\n"),
			[]byte("other\n"),
			"current $(touch${IFS}subject-dollar-marker) "+
				"`touch${IFS}subject-backtick-marker`",
			"",
		)

		cmd := lifecycleGitCommand(t, fixture.worktree, "rebase", fixture.otherRef)
		out, err := cmd.CombinedOutput()

		require.Error(err, string(out))
		assert.Equal("UU payload",
			lifecycleGit(t, fixture.worktree, "status", "--short", "--", "payload"))
		for _, marker := range markers {
			assert.NoFileExists(filepath.Join(fixture.worktree, marker))
		}
		contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
		require.NoError(err)
		for _, want := range []string{
			"<<<<<<< current\n",
			"||||||| base\n",
			"=======\n",
			">>>>>>> other\n",
			"base\n",
			"current\n",
			"other\n",
		} {
			assert.Contains(string(contents), want)
		}
		assert.NotContains(string(contents), "subject-dollar-marker")
		assert.NotContains(string(contents), "subject-backtick-marker")
	})
}

func TestUntrustedTreeMergeDriverEscapesPlaceholdersInGitPath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	markers := []string{"percent-dollar-marker", "percent-backtick-marker"}
	fixture := newMergeDriverFixture(t,
		[]byte("base\n"),
		[]byte("current\n"),
		[]byte("other\n"),
		"",
		"other-$(touch${IFS}percent-dollar-marker)-"+
			"`touch${IFS}percent-backtick-marker`",
	)
	missingGit := filepath.Join(t.TempDir(), "missing-%Y-git")
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"merge.owned.driver", safeMergeDriverCommand(missingGit,
			worktreeConfig(t, fixture.worktree, "core.hooksPath")))

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
	out, err := cmd.CombinedOutput()

	require.Error(err, string(out))
	assert.Empty(lifecycleGit(t, fixture.worktree, "status", "--short"))
	assert.Empty(lifecycleGit(t, fixture.worktree, "ls-files", "--unmerged"))
	for _, marker := range markers {
		assert.NoFileExists(filepath.Join(fixture.worktree, marker))
	}
	payload, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Equal("current\n", string(payload))
}

func TestUntrustedTreeMergeDriverFailsWhenResolvedGitDisappears(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("base\n"), []byte("current\n"), []byte("other\n"), "", "",
	)

	gitCopy := filepath.Join(t.TempDir(), "git")
	require.NoError(os.WriteFile(gitCopy, nil, 0o755))
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"merge.owned.driver", safeMergeDriverCommand(gitCopy,
			worktreeConfig(t, fixture.worktree, "core.hooksPath")))
	require.NoError(os.Remove(gitCopy))

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
	out, err := cmd.CombinedOutput()

	require.Error(err, string(out))
	assert.Empty(lifecycleGit(t, fixture.worktree, "status", "--short"))
	assert.Empty(lifecycleGit(t, fixture.worktree, "ls-files", "--unmerged"))
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Equal("current\n", string(contents))
}

func TestUntrustedTreeMergeDriverKeepsBinaryConflictLocal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("base\n"), []byte("current\n"), []byte("other\n"), "", "",
	)

	otherWorktree := filepath.Join(t.TempDir(), "other")
	lifecycleGit(t, fixture.worktree, "worktree", "add", "--detach",
		otherWorktree, fixture.otherRef)
	for _, worktree := range []string{fixture.worktree, otherWorktree} {
		require.NoError(os.WriteFile(
			filepath.Join(worktree, ".gitattributes"),
			[]byte("payload merge=owned -diff\nbinary.dat merge=owned diff=owned\n"), 0o644,
		))
	}
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"diff.owned.command", ": > classifier-diff-marker")
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"diff.owned.textconv", ": > classifier-textconv-marker")
	currentBinary := []byte("\x00current\n")
	otherBinary := []byte("\x00other\n")
	require.NoError(os.WriteFile(
		filepath.Join(fixture.worktree, "binary.dat"), currentBinary, 0o644,
	))
	lifecycleGit(t, fixture.worktree, "add", ".gitattributes", "binary.dat")
	lifecycleGit(t, fixture.worktree, "commit", "-qm", "current binary")
	require.NoError(os.WriteFile(
		filepath.Join(otherWorktree, "binary.dat"), otherBinary, 0o644,
	))
	lifecycleGit(t, otherWorktree, "add", ".gitattributes", "binary.dat")
	lifecycleGit(t, otherWorktree, "commit", "-qm", "other binary")
	otherCommit := lifecycleGit(t, otherWorktree, "rev-parse", "HEAD")
	lifecycleGit(t, fixture.worktree, "worktree", "remove", "--force", otherWorktree)

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", otherCommit)
	out, err := cmd.CombinedOutput()

	require.Error(err, string(out))
	status := strings.Split(
		lifecycleGit(t, fixture.worktree, "status", "--short"), "\n",
	)
	assert.ElementsMatch([]string{"UU binary.dat", "UU payload"}, status)
	binary, err := os.ReadFile(filepath.Join(fixture.worktree, "binary.dat"))
	require.NoError(err)
	assert.Equal(currentBinary, binary)
	assert.NotContains(string(binary), "<<<<<<<")
	payload, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Contains(string(payload), "<<<<<<< current")
	assert.Contains(string(payload), "||||||| base")
	assert.Contains(string(payload), ">>>>>>> other")
	assert.NotContains(string(out), "Cannot merge binary files: ")
	assert.NoFileExists(filepath.Join(fixture.worktree, "classifier-diff-marker"))
	assert.NoFileExists(filepath.Join(fixture.worktree, "classifier-textconv-marker"))
}

func TestUntrustedTreeMergeDriverIgnoresAmbientBigFileThreshold(t *testing.T) {
	fixture := newMergeDriverFixture(t,
		[]byte("base\n"), []byte("current\n"), []byte("other\n"), "", "",
	)
	for _, source := range []string{"command", "global"} {
		t.Run(source, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			lifecycleGit(t, fixture.worktree, "reset", "--hard", "HEAD")
			cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
			if source == "command" {
				cmd.Env = append(cmd.Env,
					"GIT_CONFIG_COUNT=1",
					"GIT_CONFIG_KEY_0=core.bigFileThreshold",
					"GIT_CONFIG_VALUE_0=1",
				)
			} else {
				globalConfig := filepath.Join(t.TempDir(), "global.gitconfig")
				require.NoError(os.WriteFile(globalConfig, []byte(
					"[core]\n\tbigFileThreshold = 1\n",
				), 0o600))
				cmd.Env = append(isolatedLifecycleBaseEnv(t),
					"GIT_CONFIG_GLOBAL="+globalConfig,
					"GIT_CONFIG_NOSYSTEM=1",
				)
			}
			out, err := cmd.CombinedOutput()

			require.Error(err, string(out))
			assert.Equal("UU payload",
				lifecycleGit(t, fixture.worktree, "status", "--short", "--", "payload"))
			contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
			require.NoError(err)
			assert.Equal(
				"<<<<<<< current\ncurrent\n||||||| base\nbase\n=======\nother\n>>>>>>> other\n",
				string(contents),
			)
		})
	}
}

func TestUntrustedTreeMergeDriverClearsInheritedRepositoryBindings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("base\n"), []byte("current\n"), []byte("other\n"), "", "",
	)

	require.NoError(os.WriteFile(
		filepath.Join(fixture.worktree, ".gitattributes"),
		[]byte("payload merge=owned\n.merge_file_* diff=owned\n"), 0o644,
	))
	lifecycleGit(t, fixture.worktree, "add", ".gitattributes")
	lifecycleGit(t, fixture.worktree, "commit", "-qm", "bind merge temp attributes")
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"diff.owned.binary", "true")
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"diff.owned.command", ": > classifier-binding-diff-marker")
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"diff.owned.textconv", ": > classifier-binding-textconv-marker")
	gitDir := lifecycleGit(t, fixture.worktree, "rev-parse", "--absolute-git-dir")
	outside := t.TempDir()
	classifier := worktreeConfig(t, fixture.worktree, "core.hooksPath")
	require.NotEmpty(classifier)

	cmd := lifecycleGitCommand(t, outside,
		"--git-dir="+gitDir,
		"--work-tree="+fixture.worktree,
		"merge", fixture.otherRef,
	)
	out, err := cmd.CombinedOutput()

	require.Error(err, string(out))
	assert.Equal("UU payload",
		lifecycleGit(t, fixture.worktree, "status", "--short", "--", "payload"))
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Equal(
		"<<<<<<< current\ncurrent\n||||||| base\nbase\n=======\nother\n>>>>>>> other\n",
		string(contents),
	)
	// The classifier runs Git with -C in the hooks directory, so a helper
	// invoked there would leave its marker in that directory rather than in
	// the worktree or the caller's directory.
	for _, dir := range []string{fixture.worktree, outside, classifier} {
		assert.NoFileExists(filepath.Join(dir, "classifier-binding-diff-marker"))
		assert.NoFileExists(filepath.Join(dir, "classifier-binding-textconv-marker"))
	}
}

func TestUntrustedTreeMergeDriverTreatsMergeFile255AsOperationError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the merge-file error simulator is a POSIX shell script")
	}
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("base\n"), []byte("current\n"), []byte("other\n"), "", "",
	)

	gitPath, err := exec.LookPath("git")
	require.NoError(err)
	gitPath, err = filepath.Abs(gitPath)
	require.NoError(err)
	simulator := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = merge-file ]; then echo simulated-output-failure >&2; exit 255; fi\n" +
		"exec " + shellquote.Single(gitPath) + " \"$@\"\n"
	require.NoError(os.WriteFile(simulator, []byte(script), 0o755))
	require.NoError(os.Chmod(simulator, 0o755))
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"merge.owned.driver", safeMergeDriverCommand(simulator,
			worktreeConfig(t, fixture.worktree, "core.hooksPath")))

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
	out, err := cmd.CombinedOutput()

	require.Error(err, string(out))
	assert.Contains(string(out), "simulated-output-failure")
	assert.Empty(lifecycleGit(t, fixture.worktree, "status", "--short"))
	assert.Empty(lifecycleGit(t, fixture.worktree, "ls-files", "--unmerged"))
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Equal("current\n", string(contents))
}

func TestUntrustedTreeMergeDriverTreatsMergeFileCrashAsOperationError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the merge-file crash simulator is a POSIX shell script")
	}
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("base\n"), []byte("current\n"), []byte("other\n"), "", "",
	)

	gitPath, err := exec.LookPath("git")
	require.NoError(err)
	gitPath, err = filepath.Abs(gitPath)
	require.NoError(err)
	simulator := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = merge-file ]; then kill -KILL $$; fi\n" +
		"exec " + shellquote.Single(gitPath) + " \"$@\"\n"
	require.NoError(os.WriteFile(simulator, []byte(script), 0o755))
	require.NoError(os.Chmod(simulator, 0o755))
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"merge.owned.driver", safeMergeDriverCommand(simulator,
			worktreeConfig(t, fixture.worktree, "core.hooksPath")))

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
	out, err := cmd.CombinedOutput()

	// A signal-killed merge process must abort the whole operation rather
	// than record a conflict whose working file holds only the current side.
	require.Error(err, string(out))
	assert.Empty(lifecycleGit(t, fixture.worktree, "status", "--short"))
	assert.Empty(lifecycleGit(t, fixture.worktree, "ls-files", "--unmerged"))
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Equal("current\n", string(contents))
}
