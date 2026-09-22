//go:build !windows

package pdfstamp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSupervisedWorkerCancellationKillsStalledProcess(t *testing.T) {
	worker := filepath.Join(t.TempDir(), "stalled-worker")
	require.NoError(t, os.WriteFile(worker, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700))
	ConfigureWorker(worker)
	t.Cleanup(func() { ConfigureWorker("") })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := SelectPagesSupervised(ctx, bytes.NewReader([]byte("synthetic source")), []int{1}, &bytes.Buffer{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
