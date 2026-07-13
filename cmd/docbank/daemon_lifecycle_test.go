//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/home"
)

func buildDocbank(t *testing.T) string {
	t.Helper()
	return buildDocbankVersion(t, "")
}

// buildDocbankVersion builds the binary, stamping internal/version.Version
// when v is non-empty (an unstamped build reports "dev").
func buildDocbankVersion(t *testing.T, v string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "docbank")
	args := []string{"build", "-tags", "fts5"}
	if v != "" {
		args = append(args, "-ldflags", "-X go.kenn.io/docbank/internal/version.Version="+v)
	}
	args = append(args, "-o", bin, "go.kenn.io/docbank/cmd/docbank")
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return bin
}

var pidRe = regexp.MustCompile(`pid (\d+)`)

// parsePID extracts the pid docbank daemon start/restart report in their
// "started"/"restarted" line, so a test can assert a restart actually
// spawned a new process rather than merely printing a fresh message.
func parsePID(t *testing.T, out string) string {
	t.Helper()
	m := pidRe.FindStringSubmatch(out)
	require.Len(t, m, 2, "expected output to contain a pid: %s", out)
	return m[1]
}

func TestLifecycleStartStatusRestartStop(t *testing.T) {
	bin := buildDocbank(t)
	dir := t.TempDir()
	env := append(os.Environ(), "DOCBANK_HOME="+dir)

	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// status/stop before any daemon: report, don't spawn.
	out, err := run("daemon", "status")
	require.Error(t, err) // exit 1 when not running
	assert.Contains(t, out, "not running")
	recs, lerr := client.RuntimeStore(dir).List()
	require.NoError(t, lerr)
	assert.Empty(t, recs, "status must not autostart")

	out, err = run("daemon", "start")
	require.NoError(t, err, out)
	t.Cleanup(func() { _, _ = client.Stop(context.Background(), dir) })
	startPID := parsePID(t, out)
	out, err = run("daemon", "status")
	require.NoError(t, err, out)
	assert.Contains(t, out, "running")

	out, err = run("ls", "/")
	require.NoError(t, err, out)

	out, err = run("daemon", "restart")
	require.NoError(t, err, out)
	assert.Contains(t, out, "restarted")
	restartPID := parsePID(t, out)
	assert.NotEqual(t, startPID, restartPID, "restart must spawn a new daemon process")

	out, err = run("daemon", "stop")
	require.NoError(t, err, out)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, found, err := client.Find(ctx, dir)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestDaemonStartDoesNotInitializeRestoreOwnedMissingTarget(t *testing.T) {
	bin := buildDocbank(t)
	parent := filepath.Join(t.TempDir(), "restore-target")
	require.NoError(t, os.Mkdir(parent, 0o700))
	lock, err := (home.Layout{Root: parent}).TryLockExclusive()
	require.NoError(t, err)
	defer func() { _ = lock.Release() }()

	target := filepath.Join(parent, "docbank.db")
	cmd := exec.Command(bin, "daemon", "start")
	cmd.Env = append(os.Environ(), "DOCBANK_HOME="+target)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, string(out))
	assert.Contains(t, string(out), "vault is locked",
		"the external bootstrap capture must preserve the child startup error")
	_, err = os.Lstat(target)
	require.ErrorIs(t, err, os.ErrNotExist,
		"the launcher and failed child must not initialize a restore-owned target")
}

func TestDaemonStartReplacesIncompatibleDaemon(t *testing.T) {
	oldBin := buildDocbank(t) // reports version "dev"
	newBin := buildDocbankVersion(t, "v9.9.9-test")
	dir := t.TempDir()
	env := append(os.Environ(), "DOCBANK_HOME="+dir)

	run := func(bin string, args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run(oldBin, "daemon", "start")
	require.NoError(t, err, out)
	t.Cleanup(func() { _, _ = client.Stop(context.Background(), dir) })
	oldPID := parsePID(t, out)

	// A same-version start leaves the running daemon alone.
	out, err = run(oldBin, "daemon", "start")
	require.NoError(t, err, out)
	assert.Contains(t, out, "already running")
	assert.Equal(t, oldPID, parsePID(t, out))

	// Initialize the repository before making the runtime record stale: the
	// following backup create must replace that daemon before it calls the new
	// progress-stream endpoint.
	repoPath := filepath.Join(t.TempDir(), "backup")
	out, err = run(oldBin, "backup", "init", "--repo", repoPath)
	require.NoError(t, err, out)
	assert.Contains(t, out, "initialized backup repository")

	// Simulate the immediately preceding same-version dev protocol. Data
	// commands share daemon start's convergence path, so backup create must
	// replace it instead of reaching the old daemon and failing with 404.
	recs, err := client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "7", recs[0].Metadata["protocol_version"])
	recs[0].Metadata["protocol_version"] = "4"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)

	out, err = run(oldBin, "backup", "create", "--repo", repoPath, "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "freeze:")
	assert.Contains(t, out, "created snapshot")
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	protocolPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, oldPID, protocolPID)

	// A mismatched-version start stops the stale daemon and starts its own.
	out, err = run(newBin, "daemon", "start")
	require.NoError(t, err, out)
	assert.Contains(t, out, "replaced daemon dev (pid "+protocolPID+") with v9.9.9-test")
	pids := pidRe.FindAllStringSubmatch(out, -1) // old pid, then the new daemon's
	require.Len(t, pids, 2, out)
	newPID := pids[1][1]
	assert.NotEqual(t, protocolPID, newPID)

	out, err = run(newBin, "daemon", "status")
	require.NoError(t, err, out)
	assert.Contains(t, out, "v9.9.9-test")
}

func TestRestartWhenNotRunningStartsFresh(t *testing.T) {
	bin := buildDocbank(t)
	// A path that does not exist yet: first-run restart must treat the
	// missing home as "not running", not fail during discovery.
	dir := filepath.Join(t.TempDir(), "home")
	env := append(os.Environ(), "DOCBANK_HOME="+dir)

	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("daemon", "restart")
	require.NoError(t, err, out)
	t.Cleanup(func() { _, _ = client.Stop(context.Background(), dir) })
	assert.Contains(t, out, "started (was not running)")
}
