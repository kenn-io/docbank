//go:build windows

package client

import (
	"math"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestProcessTerminationTreatsMissingProcessAsStopped(t *testing.T) {
	require.NoError(t, requestProcessStop(math.MaxInt32))
	require.NoError(t, forceTerminateProcess(math.MaxInt32))
}

func TestGracefulStopDoesNotKillWindowsProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsGracefulStopHelper$")
	cmd.Env = append(os.Environ(), "DOCBANK_WINDOWS_STOP_HELPER=1")
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("helper process did not exit")
		}
	})
	require.Eventually(t, func() bool {
		return kitdaemon.ProcessAlive(cmd.Process.Pid)
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, requestProcessStop(cmd.Process.Pid))
	time.Sleep(100 * time.Millisecond)
	assert.True(t, kitdaemon.ProcessAlive(cmd.Process.Pid),
		"graceful Windows fallback must wait rather than terminate")
}

func TestWindowsGracefulStopHelper(_ *testing.T) {
	if os.Getenv("DOCBANK_WINDOWS_STOP_HELPER") == "1" {
		time.Sleep(30 * time.Second)
	}
}
