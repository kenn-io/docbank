//go:build !windows

package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactionVerifyRejectsFIFO(t *testing.T) {
	f := newRedactionVerifyFixture(t)
	// Opening a FIFO for reading waits for a writer. Without read permission
	// the open fails at once, so verify reports "permission denied" instead
	// of "regular file" if it ever opens the input, and the test needs no
	// deadline. A user who bypasses file permissions, such as root, would
	// instead wait in that open until the test binary times out.
	fifo := filepath.Join(t.TempDir(), "input.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o200))
	for _, test := range []struct {
		name  string
		index int
	}{{"preparation", 2}, {"archive", 5}} {
		t.Run(test.name, func(t *testing.T) {
			args := slices.Clone(f.args)
			args[test.index] = fifo
			resetFlags(rootCmd)
			var stdout, stderr bytes.Buffer
			code := runProcess(append(args, "--json"), &stdout, &stderr)
			require.Equal(t, exitUsage, code, "%s", stderr.String())
			require.Contains(t, stderr.String(), "regular file")
			require.Empty(t, stdout.String())
		})
	}
}
