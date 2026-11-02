package openssh

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const cancellationHelperMarker = "KIT_OPENSSH_CANCELLATION_HELPER_MARKER"

func TestCancellationHelperProcess(t *testing.T) {
	marker := os.Getenv(cancellationHelperMarker)
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		os.Exit(2)
	}
	time.Sleep(time.Minute) //nolint:kennlint // sleeptest: helper process must stay alive until it is killed
}

func TestRunOutputPreservesContextCancellation(t *testing.T) {
	err := runCancellationHelper(t, func(
		ctx context.Context,
		executable string,
		arguments []string,
	) error {
		_, _, _, runErr := runOutput(
			ctx, append([]string{executable}, arguments...),
		)
		return runErr
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunSSHCommandPreservesContextCancellation(t *testing.T) {
	err := runCancellationHelper(t, func(
		ctx context.Context,
		executable string,
		arguments []string,
	) error {
		_, runErr := runSSHCommand(ctx, executable, arguments)
		return runErr
	})
	require.ErrorIs(t, err, context.Canceled)
}

func runCancellationHelper(
	t *testing.T,
	run func(context.Context, string, []string) error,
) error {
	t.Helper()
	require := require.New(t)
	executable, err := os.Executable()
	require.NoError(err)
	marker := t.TempDir() + "/ready"
	t.Setenv(cancellationHelperMarker, marker)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resultChannel := make(chan error, 1)
	go func() {
		resultChannel <- run(ctx, executable, []string{
			"-test.run=^TestCancellationHelperProcess$",
		})
	}()
	require.Eventually(func() bool {
		_, statErr := os.Stat(marker)
		return statErr == nil
	}, 5*time.Second, 5*time.Millisecond)
	cancel()
	return <-resultChannel
}
