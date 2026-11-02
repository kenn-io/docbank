//go:build windows

package daemon_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

// TestStartDetachedConsoleInputHelper is not a real test:
// TestStartDetachedDoesNotExposeConsoleInput re-executes the test binary with
// -test.run targeting it so the detached child can report whether CONIN$
// exists in its own process.
func TestStartDetachedConsoleInputHelper(t *testing.T) {
	marker := os.Getenv("KIT_DAEMON_TEST_CONSOLE_MARKER")
	if marker == "" {
		t.Skip("helper process for TestStartDetachedDoesNotExposeConsoleInput")
	}
	file, err := os.OpenFile("CONIN$", os.O_RDONLY, 0)
	if err == nil {
		_ = file.Close()
		require.NoError(t, os.WriteFile(marker, []byte("console-input-present"), 0o600))
		return
	}
	require.NoError(t, os.WriteFile(marker, []byte("console-input-unavailable"), 0o600))
}

func TestStartDetachedDoesNotExposeConsoleInput(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "console-marker")

	err = daemon.StartDetached(t.Context(), daemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"-test.run", "^TestStartDetachedConsoleInputHelper$"},
		Env:        append(os.Environ(), "KIT_DAEMON_TEST_CONSOLE_MARKER="+marker),
	})
	require.NoError(t, err)

	got := waitForMarker(t, marker)
	require.Equal(t, "console-input-unavailable", strings.TrimSpace(got))
}

func waitForMarker(t *testing.T, marker string) string {
	t.Helper()
	var data []byte
	require.Eventually(t, func() bool {
		var err error
		data, err = os.ReadFile(marker)
		return err == nil
	}, 10*time.Second, 25*time.Millisecond, "detached child never wrote marker file")
	return string(data)
}
