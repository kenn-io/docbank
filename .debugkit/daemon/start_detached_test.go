package daemon_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

// TestStartDetachedHelper is not a real test: TestStartDetachedRunsChild
// re-executes the test binary with -test.run targeting it so the detached
// child has observable work to do.
func TestStartDetachedHelper(t *testing.T) {
	marker := os.Getenv("KIT_DAEMON_TEST_MARKER")
	if marker == "" {
		t.Skip("helper process for TestStartDetachedRunsChild")
	}
	require.NoError(t, os.WriteFile(marker, []byte("ok"), 0o600))
}

func TestStartDetachedRunsChild(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "marker")

	err = daemon.StartDetached(t.Context(), daemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"-test.run", "^TestStartDetachedHelper$"},
		Env:        append(os.Environ(), "KIT_DAEMON_TEST_MARKER="+marker),
	})
	require.NoError(t, err)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		require.True(t, time.Now().Before(deadline), "detached child never wrote marker file")
		time.Sleep(25 * time.Millisecond) //nolint:kennlint // sleeptest: polls a marker written by a detached child process
	}
}
