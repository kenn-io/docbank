package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestWebRestartRetriesAfterHelperFailure(t *testing.T) {
	var exits []func(error)
	start := func(_ context.Context, opts kitdaemon.StartDetachedOptions) error {
		exits = append(exits, opts.Exited)
		return nil
	}
	restart := webRestartLauncher(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), start)

	require.NoError(t, restart(t.Context(), false))
	require.NoError(t, restart(t.Context(), false))
	require.Len(t, exits, 1)
	exits[0](errors.New("helper failed"))
	require.NoError(t, restart(t.Context(), false))
	require.Len(t, exits, 2)
}
