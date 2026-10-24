//go:build !windows

package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRedactionVerifyRejectsFIFO(t *testing.T) {
	f := newRedactionVerifyFixture(t)
	f.archive(t, "valid")
	binary := buildDocbank(t)
	fifo := filepath.Join(t.TempDir(), "input.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))
	for _, test := range []struct {
		name  string
		index int
	}{{"preparation", 2}, {"archive", 5}} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string(nil), f.args...)
			args[test.index] = fifo
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, append(args, "--json")...)
			output, err := command.Output()
			require.NoError(t, ctx.Err(), "verify blocked opening a FIFO")
			var failure *exec.ExitError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, exitUsage, failure.ExitCode())
			require.Contains(t, string(failure.Stderr), "regular file")
			require.Empty(t, output)
		})
	}
}
