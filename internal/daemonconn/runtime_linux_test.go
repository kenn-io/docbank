package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

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
