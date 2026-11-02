package daemon_test

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

// TestStartDetachedExitHelper is not a real test:
// TestStartDetachedReportsChildExit re-executes the test binary with
// -test.run targeting it so the detached child exits with a known status.
func TestStartDetachedExitHelper(t *testing.T) {
	if os.Getenv("KIT_DAEMON_TEST_EXIT_HELPER") == "" {
		t.Skip("helper process for TestStartDetachedReportsChildExit")
	}
	os.Exit(3)
}

func TestStartDetachedReportsChildExit(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	exited := make(chan error, 1)

	err = daemon.StartDetached(t.Context(), daemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"-test.run", "^TestStartDetachedExitHelper$"},
		Env:        append(os.Environ(), "KIT_DAEMON_TEST_EXIT_HELPER=1"),
		Exited:     func(err error) { exited <- err },
	})
	require.NoError(t, err)

	select {
	case err := <-exited:
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 3, exitErr.ExitCode())
	case <-time.After(10 * time.Second):
		require.Fail(t, "Exited was not called after the child exited")
	}
}
