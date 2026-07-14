//go:build !windows

package client

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestForceTerminateKillsProcessIgnoringTerm(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	//nolint:gosec // exact current test executable, no shell or user command text.
	cmd := exec.Command(os.Args[0], "-test.run=^TestIgnoreTermHelper$")
	cmd.Env = append(os.Environ(),
		"DOCBANK_IGNORE_TERM_HELPER=1", "DOCBANK_HELPER_READY="+ready)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, requestProcessStop(cmd.Process.Pid))
	time.Sleep(100 * time.Millisecond)
	assert.True(t, kitdaemon.ProcessAlive(cmd.Process.Pid), "helper must ignore SIGTERM")
	require.NoError(t, forceTerminateProcess(cmd.Process.Pid))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err, "SIGKILL should produce a non-success exit")
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit after SIGKILL")
	}
}

func TestIgnoreTermHelper(_ *testing.T) {
	if os.Getenv("DOCBANK_IGNORE_TERM_HELPER") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	if err := os.WriteFile(os.Getenv("DOCBANK_HELPER_READY"), []byte("ready"), 0o600); err != nil {
		os.Exit(2)
	}
	select {}
}
