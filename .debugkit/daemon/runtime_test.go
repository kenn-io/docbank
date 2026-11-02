package daemon_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/fsname"
)

func TestRuntimeStoreWriteListAndRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	ep := daemon.Endpoint{Network: daemon.NetworkTCP, Address: "127.0.0.1:7474"}
	rec := daemon.NewRuntimeRecord("kata", "v1", ep)
	rec.StartedAt = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	rec.Metadata = map[string]string{"db_path": "/tmp/kata.db"}

	path, err := store.Write(rec)
	require.NoError(err)
	assert.Equal("daemon."+strconv.Itoa(os.Getpid())+".json", filepath.Base(path))

	records, err := store.List()
	require.NoError(err)
	require.Len(records, 1)
	got := records[0]
	assert.Equal(os.Getpid(), got.PID)
	assert.Equal("kata", got.Service)
	assert.Equal("v1", got.Version)
	assert.Equal(ep, got.Endpoint())
	assert.Equal(path, got.SourcePath)
	assert.Equal("/tmp/kata.db", got.Metadata["db_path"])
}

func TestNewRuntimeRecordPersistsCurrentProcessIdentity(t *testing.T) {
	rec := daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1234",
	})
	assert.Equal(t, daemon.ProcessIdentityMatch,
		daemon.CompareRuntimeProcessIdentity(rec))
}

func TestRuntimeStoreCleanupDeadLeavesMismatchedFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	pid := deadPID(t)
	dead := daemon.RuntimeRecord{
		PID:       pid,
		Network:   daemon.NetworkTCP,
		Address:   "127.0.0.1:7474",
		StartedAt: time.Now(),
	}
	_, err := store.Write(dead)
	require.NoError(err)

	mismatchPath, err := store.Path(pid + 1)
	require.NoError(err)
	err = os.WriteFile(mismatchPath, []byte(`{"pid":`+strconv.Itoa(pid)+`,"address":"127.0.0.1:7475"}`), 0o644)
	require.NoError(err)

	removed, err := store.CleanupDead()
	require.NoError(err)
	assert.Equal(1, removed)
	deadPath, err := store.Path(pid)
	require.NoError(err)
	_, err = os.Stat(deadPath)
	assert.True(os.IsNotExist(err), "dead runtime still exists or unexpected stat error: %v", err)
	_, err = os.Stat(mismatchPath)
	require.NoError(err)
}

func TestRuntimeStoreRejectsPrefixTraversal(t *testing.T) {
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "../escape"}

	_, err := store.Path(123)
	require.Error(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:     123,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	})
	require.Error(err)
	_, err = store.List()
	require.Error(err)
	_, err = store.CleanupDead()
	require.Error(err)
}

func TestRuntimeStoreRejectsRelativeDirBeforePreparing(t *testing.T) {
	assert := assert.New(t)

	store := daemon.RuntimeStore{Dir: "relative-runtime"}

	_, err := store.LockPath()
	require.Error(t, err)
	assert.Contains(err.Error(), "must be absolute")

	_, statErr := os.Stat("relative-runtime")
	assert.True(os.IsNotExist(statErr), "relative runtime dir should not be created: %v", statErr)
}

func TestRuntimeStoreListenLockPathIsSeparateFromStartLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "kata"}

	startLock, err := store.LockPath()
	require.NoError(err)
	listenLock, err := store.ListenLockPath()
	require.NoError(err)

	assert.Equal(filepath.Join(store.Dir, "kata.lock"), startLock)
	assert.Equal(filepath.Join(store.Dir, "kata.listen.lock"), listenLock)
}

func TestRuntimeStoreCheckWritableLeavesNoFiles(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	store := daemon.RuntimeStore{Dir: dir}

	require.NoError(store.CheckWritable())
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	require.Empty(entries)
	records, err := store.List()
	require.NoError(err)
	require.Empty(records)
}

func TestRuntimeStoreCheckWritableRejectsNonDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "runtime-file")
	require.NoError(os.WriteFile(path, []byte("not a directory"), 0o600))

	err := (daemon.RuntimeStore{Dir: path}).CheckWritable()

	require.Error(err)
	assert.Contains(err.Error(), "prepare runtime dir")
}

func TestRuntimeStoreIgnoresStagingFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	pid := deadPID(t)
	// An interrupted Write leaves its staging file behind under this name.
	staging := filepath.Join(store.Dir, ".daemon."+strconv.Itoa(pid)+".json.tmp-123456")
	body := []byte(`{"pid":` + strconv.Itoa(pid) + `,"address":"127.0.0.1:7474"}`)
	require.NoError(os.WriteFile(staging, body, 0o644))

	records, err := store.List()
	require.NoError(err)
	assert.Empty(records)
	removed, err := store.CleanupDead()
	require.NoError(err)
	assert.Zero(removed)
	_, err = os.Stat(staging)
	require.NoError(err)

	_, err = store.Write(daemon.RuntimeRecord{PID: pid, Address: "127.0.0.1:7475"})
	require.NoError(err)
	entries, err := os.ReadDir(store.Dir)
	require.NoError(err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.ElementsMatch([]string{".daemon." + strconv.Itoa(pid) + ".json.tmp-123456", "daemon." + strconv.Itoa(pid) + ".json"}, names)
}

func TestRuntimeStoreRejectsNonPortablePrefix(t *testing.T) {
	for _, prefix := range []string{"a:b", "NUL", "con", "CONIN$", "tool.", "tool "} {
		t.Run(prefix, func(t *testing.T) {
			store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: prefix}

			_, err := store.Path(123)

			require.ErrorIs(t, err, fsname.ErrNotPortable)
		})
	}
}

func TestRuntimeStorePrefixReservesPortableStagingSuffix(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	store := daemon.RuntimeStore{Dir: dir, Prefix: strings.Repeat("a", 214)}

	path, err := store.Write(daemon.RuntimeRecord{
		PID:     1,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	})
	require.NoError(err)
	_, err = os.Stat(path)
	require.NoError(err)

	store.Prefix = strings.Repeat("a", 215)
	_, err = store.Path(1)
	require.ErrorIs(err, fsname.ErrNotPortable)
}

func TestRuntimeProcessAliveKeepsRecordWithUnreadableVersionedIdentity(t *testing.T) {
	rec := daemon.RuntimeRecord{
		PID:               deadPID(t),
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:7474",
		ProcessIdentityV2: "future-v1:12345",
	}
	assert.True(t, daemon.RuntimeProcessAlive(rec))
}

func TestCleanupDeadKeepsRecordWithUnjudgedIdentity(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	path, err := store.Write(daemon.RuntimeRecord{
		PID:               deadPID(t),
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:7474",
		Service:           "tool",
		StartedAt:         time.Now(),
		ProcessIdentityV2: "future-v1:12345",
	})
	require.NoError(t, err)

	removed, err := store.CleanupDead()
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.FileExists(t, path)
}

func TestDiscoverProbesRecordWithUnjudgedIdentity(t *testing.T) {
	pid := deadPID(t)
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: "tool",
		PID:     pid,
	}))
	defer server.Close()
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:               pid,
		Network:           daemon.NetworkTCP,
		Address:           listenerAddr(t, server),
		Service:           "tool",
		StartedAt:         time.Now(),
		ProcessIdentityV2: "future-v1:12345",
	})
	require.NoError(t, err)

	rec, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
		RequirePIDAlive: true,
	})
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, pid, rec.PID)
}

// deadPID returns a PID that names no live process, or skips the test.
func deadPID(t *testing.T) int {
	t.Helper()
	const pid = 999999
	if daemon.ProcessAlive(pid) {
		t.Skipf("pid %d is alive on this host", pid)
	}
	return pid
}
