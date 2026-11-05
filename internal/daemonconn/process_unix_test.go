//go:build !windows

package daemonconn

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

const (
	ignoreTermReady    = "ignore-term-ready"
	ignoreTermReceived = "ignore-term-received"
)

func TestForceTerminateKillsProcessIgnoringTerm(t *testing.T) {
	//nolint:gosec // exact current test executable, no shell or user command text.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestIgnoreTermHelper$")
	cmd.Env = append(os.Environ(), "DOCBANK_IGNORE_TERM_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	lines := bufio.NewReader(stdout)
	line, err := lines.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, ignoreTermReady+"\n", line)

	require.NoError(t, requestProcessStop(cmd.Process.Pid))
	// The helper reports the SIGTERM it received and keeps running, so the
	// stop request sent SIGTERM and not a signal that kills.
	line, err = lines.ReadString('\n')
	require.NoError(t, err, "the helper must outlive SIGTERM")
	require.Equal(t, ignoreTermReceived+"\n", line)
	assert.True(t, kitdaemon.ProcessAlive(cmd.Process.Pid), "helper must ignore SIGTERM")
	require.NoError(t, forceTerminateProcess(cmd.Process.Pid))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err, "SIGKILL should produce a non-success exit")
		status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		require.True(t, ok)
		assert.Equal(t, syscall.SIGKILL, status.Signal())
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit after SIGKILL")
	}
}

// TestIgnoreTermHelper keeps running through SIGTERM and reports each one.
func TestIgnoreTermHelper(_ *testing.T) {
	if os.Getenv("DOCBANK_IGNORE_TERM_HELPER") != "1" {
		return
	}
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	fmt.Println(ignoreTermReady)
	for range terms {
		fmt.Println(ignoreTermReceived)
	}
}
