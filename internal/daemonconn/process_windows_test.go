//go:build windows

package daemonconn

import (
	"io"
	"math"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessTerminationTreatsMissingProcessAsStopped(t *testing.T) {
	require.NoError(t, requestProcessStop(math.MaxInt32))
	require.NoError(t, forceTerminateProcess(math.MaxInt32))
}

const windowsStopHelperExited = "windows-stop-helper-exited-normally"

func TestGracefulStopDoesNotKillWindowsProcess(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWindowsGracefulStopHelper$") // #nosec G702 -- os.Args[0] is the trusted current test executable.
	cmd.Env = append(os.Environ(), "DOCBANK_WINDOWS_STOP_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	require.NoError(t, requestProcessStop(cmd.Process.Pid))
	// The helper exits normally once its input closes. A stop request that
	// terminated it would end it with a failure code and without the marker.
	require.NoError(t, stdin.Close())
	output, err := io.ReadAll(stdout)
	require.NoError(t, err)
	require.NoError(t, cmd.Wait(), "graceful Windows fallback must wait rather than terminate")
	assert.Contains(t, string(output), windowsStopHelperExited)
}

// TestWindowsGracefulStopHelper runs until its input closes.
func TestWindowsGracefulStopHelper(_ *testing.T) {
	if os.Getenv("DOCBANK_WINDOWS_STOP_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	_, _ = os.Stdout.WriteString(windowsStopHelperExited + "\n")
}
