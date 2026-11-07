package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	"golang.org/x/sys/unix"

	"go.kenn.io/docbank/internal/daemon"
)

func TestWaitDeadReturnsForUnreapedChild(t *testing.T) {
	//nolint:gosec // Run the existing helper in the current test executable.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUnresponsiveDaemonHelper$")
	cmd.Env = append(os.Environ(), "DOCBANK_UNRESPONSIVE_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	require.NoError(t, stdin.Close())

	// Observe the exit without reaping the child, as in a container without init.
	var waitErr error
	require.Eventually(t, func() bool {
		var info unix.Siginfo
		waitErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		return waitErr != nil || info.Signo != 0
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, waitErr)

	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		dead, err := waitDead(t.Context(), kitdaemon.RuntimeRecord{PID: cmd.Process.Pid}, daemon.GracefulExitTimeout)
		require.NoError(t, err)
		require.True(t, dead, "an exited child must finish the daemon stop wait")
		require.Zero(t, time.Since(started), "an unreaped child must end the wait at once")
	})
}

func TestDaemonDiscoveryAfterBootTimeChange(t *testing.T) {
	// Keep the real process start ticks while changing the boot-time estimate
	// used by gopsutil. Docker derives that estimate from wall time minus uptime.
	proc := t.TempDir()
	pid := strconv.Itoa(os.Getpid())
	require.NoError(t, os.Mkdir(filepath.Join(proc, pid), 0700))
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(proc, pid, "stat"), stat, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "stat"), []byte("btime 1600000000\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "uptime"), []byte("100000.00 0.00\n"), 0600))
	t.Setenv("HOST_PROC", proc)

	const token = "synthetic-boot-time-proof-token"
	var rec kitdaemon.RuntimeRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == kitdaemon.DefaultPingPath {
			_ = json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: rec.Service, Version: rec.Version, PID: rec.PID})
			return
		}
		_, _ = w.Write(challengeResponse(t, r, token))
	}))
	t.Cleanup(server.Close)
	rec = NewRecord(strings.TrimPrefix(server.URL, "http://"), "synthetic-key", token, "")
	root := t.TempDir()
	_, err = RuntimeStore(root).Write(rec)
	require.NoError(t, err)
	_, _, found, err := Find(t.Context(), root)
	require.NoError(t, err)
	require.True(t, found)

	require.NoError(t, os.WriteFile(filepath.Join(proc, "stat"), []byte("btime 1600000001\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "uptime"), []byte("99999.00 0.00\n"), 0600))
	_, _, found, err = Find(t.Context(), root)
	require.NoError(t, err)
	require.True(t, found, "the same live daemon must remain discoverable")
	require.NoError(t, verifyRecordProcess(rec))
	_, live, err := liveRuntimeRecord(root)
	require.NoError(t, err)
	require.True(t, live, "a draining daemon must retain its process identity")
}
