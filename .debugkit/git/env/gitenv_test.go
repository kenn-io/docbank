package gitenv

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripAllRemovesEveryGitVariable(t *testing.T) {
	env := []string{
		"PATH=/bin",
		"GIT_DIR=/parent/.git",
		"GIT_SSL_NO_VERIFY=1",
		"GIT_DEFAULT_HASH=sha256",
		"SSH_ASKPASS=/tmp/askpass",
	}

	got := StripAll(env)

	if slices.Contains(got, "PATH=/bin") == false {
		require.FailNow(t, fmt.Sprintf("PATH should be preserved: %#v", got))
	}
	for _, removed := range env[1:] {
		if slices.Contains(got, removed) {
			require.FailNow(t, fmt.Sprintf("%q should have been removed from %#v", removed, got))
		}
	}
}

func TestStripAllForGOOSRemovesLowercaseWindowsGitVariables(t *testing.T) {
	env := []string{
		"PATH=C:\\Windows",
		"git_dir=C:\\parent\\.git",
		"git_work_tree=C:\\parent",
		"git_config_parameters='core.worktree'='C:\\elsewhere'",
		"ssh_askpass=C:\\askpass.exe",
	}

	got := stripAllForGOOS(env, "windows")

	if !slices.Equal(got, []string{"PATH=C:\\Windows"}) {
		require.FailNow(t, fmt.Sprintf("Windows Git variables were not stripped: %#v", got))
	}
}

func TestStripInheritedPreservesDiagnosticsButRemovesContext(t *testing.T) {
	env := []string{
		"GIT_DIR=/parent/.git",
		"GIT_CONFIG_COUNT=1",
		"GIT_TRACE=1",
		"GIT_SSL_NO_VERIFY=1",
	}

	got := StripInherited(env)

	if slices.Contains(got, "GIT_DIR=/parent/.git") {
		require.FailNow(t, fmt.Sprintf("GIT_DIR should have been removed: %#v", got))
	}
	if slices.Contains(got, "GIT_CONFIG_COUNT=1") {
		require.FailNow(t, fmt.Sprintf("GIT_CONFIG_COUNT should have been removed: %#v", got))
	}
	if !slices.Contains(got, "GIT_TRACE=1") || !slices.Contains(got, "GIT_SSL_NO_VERIFY=1") {
		require.FailNow(t, fmt.Sprintf("diagnostic/transport env should be preserved: %#v", got))
	}
}
