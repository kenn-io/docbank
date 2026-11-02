package daemon_test

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestTryAcquireStartLockCoordinatesWithManager(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	release, acquired, err := store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired)
	require.NotNil(release)
	// Keep cleanup valid even if a precondition below fails.
	defer func() {
		if release != nil {
			release()
		}
	}()

	again, acquired, err := store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.False(acquired)
	require.Nil(again)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.AcquireStartLock(ctx)
	require.ErrorIs(err, context.Canceled)

	snapshot := filepath.Join(store.Dir, "progress.json")
	require.NoError(os.WriteFile(snapshot, []byte("old progress"), 0o600))
	ready := false
	manager := daemon.Manager{
		Store: store,
		FindFunc: func(context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
			return daemon.RuntimeRecord{}, daemon.PingInfo{}, ready, nil
		},
		Start: func(ctx context.Context) error {
			// Manager owns the same local lock while invoking Start.
			probeRelease, acquired, err := store.TryAcquireStartLock(ctx)
			require.
				NoError(err)
			assert.False(acquired)
			assert.Nil(probeRelease)
			assert.NoFileExists(snapshot)
			ready = true
			return nil
		},
	}
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; the test holds the start lock, so Ensure can only stop when it fires
	defer cancel()
	_, _, err = manager.Ensure(ctx, time.Second)
	require.ErrorIs(err, context.DeadlineExceeded)
	require.False(ready)
	require.NoError(os.Remove(snapshot)) // Caller-owned cleanup happens under lock.
	release()
	release = nil
	_, _, err = manager.Ensure(t.Context(), time.Second)
	require.NoError(err)
	require.True(ready)
	release, acquired, err = store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired)
}

func TestTryAcquireStartLockErrorAndRetry(t *testing.T) {
	require := require.New(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	path, err := store.LockPath()
	require.NoError(err)
	// Windows rejects opening a directory here. Unix permits locking one,
	// so use an unreadable file to exercise its open error instead.
	if runtime.GOOS == "windows" {
		require.NoError(os.Mkdir(path, 0o700))
	} else {
		if os.Geteuid() == 0 {
			t.Skip("root can open mode-000 files")
		}
		require.NoError(os.WriteFile(path, nil, 0o000))
	}
	release, acquired, err := store.TryAcquireStartLock(t.Context())
	require.Error(err)
	require.Nil(release)
	require.False(acquired)
	require.NoError(os.Remove(path))
	release, acquired, err = store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired)
	require.NotNil(release)
	release()
}

func TestTryAcquireStartLockCanceled(t *testing.T) {
	require := require.New(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	release, acquired, err := store.TryAcquireStartLock(ctx)
	require.ErrorIs(err, context.Canceled)
	require.Nil(release)
	require.False(acquired)
	release, acquired, err = store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired)
	release()
}

func TestTryAcquireStartLockAfterHolderExit(t *testing.T) {
	require := require.New(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	exe, err := os.Executable()
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestStartLockProcess$", "--", store.Dir)
	stdout, err := cmd.StdoutPipe()
	require.NoError(err)
	stdin, err := cmd.StdinPipe()
	require.NoError(err)
	defer stdin.Close()
	require.NoError(cmd.Start())
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(err)
	require.Equal("locked\n", line)
	release, acquired, err := store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.False(acquired)
	require.Nil(release)
	require.NoError(cmd.Process.Kill())
	require.Error(cmd.Wait())
	waited = true
	path, err := store.LockPath()
	require.NoError(err)
	require.FileExists(path)
	release, acquired, err = store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired, "a terminated holder's remaining file does not block acquisition")
	require.NotNil(release)
	release()
}

func TestStartLockProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	store := daemon.RuntimeStore{Dir: os.Args[len(os.Args)-1]}
	release, err := store.AcquireStartLock(t.Context())
	require.NoError(t, err)
	defer release()
	_, err = os.Stdout.WriteString("locked\n")
	require.NoError(t, err)
	var buf [1]byte
	_, _ = os.Stdin.Read(buf[:])
}
