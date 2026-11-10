//go:build !windows

package pdfstamp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupervisedWorkerCancellationKillsStalledProcess(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	require.NoError(t, syscall.Mkfifo(started, 0o600))
	worker := filepath.Join(dir, "stalled-worker")
	script := "#!/bin/sh\necho $$ > '" + started + "'\nexec sleep 30\n"
	require.NoError(t, os.WriteFile(worker, []byte(script), 0o700))
	ConfigureWorker(worker)
	t.Cleanup(func() { ConfigureWorker("") })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := SelectPagesSupervised(ctx, bytes.NewReader([]byte("synthetic source")), []int{1}, &bytes.Buffer{})
		result <- err
	}()

	// Reading the FIFO blocks until the worker writes its PID, so the
	// cancellation below always reaches a running, stalled process.
	reported := make(chan []byte, 1)
	go func() {
		text, _ := os.ReadFile(started)
		reported <- text
	}()
	var pidText []byte
	select {
	case pidText = <-reported:
	case err := <-result:
		t.Fatalf("worker returned before it stalled: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	require.NoError(t, err)
	cancel()

	require.ErrorIs(t, <-result, context.Canceled)
	require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "the stalled worker must be killed and reaped")
}
