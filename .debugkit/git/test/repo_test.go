package gittest

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewRepoIgnoresPollutedGitEnvironment(t *testing.T) {
	parent := NewRepoWithCommit(t)
	t.Setenv("GIT_DIR", parent.GitDir)
	t.Setenv("GIT_WORK_TREE", parent.Root)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Polluted")

	repo := NewRepoWithCommit(t)

	if repo.Root == parent.Root {
		require.FailNow(t, "expected a distinct test repo")
	}
	if got := repo.Run("config", "user.name"); got != UserName {
		require.FailNow(t, fmt.Sprintf("user.name = %q, want %q", got, UserName))
	}
	if got := filepath.Clean(repo.Run("rev-parse", "--show-toplevel")); got != filepath.Clean(repo.Root) {
		require.FailNow(t, fmt.Sprintf("show-toplevel = %q, want %q", got, repo.Root))
	}
}
