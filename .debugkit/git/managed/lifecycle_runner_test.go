package managedworktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestCreateWorktreeReusesTrustAfterChangingLocale(t *testing.T) {
	t.Parallel()
	root := initLifecycleRepo(t)
	trace := filepath.Join(t.TempDir(), "trace")
	runner := gitcmd.New()
	runner.Env = append(lifecycleGitEnv(t), "LC_ALL=fr_FR.UTF-8", "GIT_TRACE="+filepath.ToSlash(trace))
	// Callers can use their runner before handing it to the lifecycle. Its
	// trust cache then belongs to the caller's original environment.
	_, err := runner.Output(t.Context(), root, "rev-parse", "HEAD")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(trace, nil, 0o600))

	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: runner,
	})
	require.NoError(t, err)
	contents, err := os.ReadFile(trace)
	require.NoError(t, err)
	// StripEnv hides GIT_TRACE from lifecycle commands, while the native
	// trust probes retain it. One initial global read is enough; subsequent
	// commands can reuse it after discovering the config files.
	assert.Equal(t, 1, strings.Count(string(contents), "config --global --includes"), "%s", contents)
	assert.Equal(t, "topic", lifecycleGit(t, created.Path, "branch", "--show-current"))
	assert.Equal(t, lifecycleGit(t, root, "rev-parse", "HEAD"), lifecycleGit(t, created.Path, "rev-parse", "HEAD"))
}
