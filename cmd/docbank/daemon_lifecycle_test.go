//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/client"
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

func TestDaemonStartReplacesVersionMismatchedDaemon(t *testing.T) {
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

	// A mismatched-version start stops the stale daemon and starts its own.
	out, err = run(newBin, "daemon", "start")
	require.NoError(t, err, out)
	assert.Contains(t, out, "replaced daemon dev (pid "+oldPID+") with v9.9.9-test")
	pids := pidRe.FindAllStringSubmatch(out, -1) // old pid, then the new daemon's
	require.Len(t, pids, 2, out)
	newPID := pids[1][1]
	assert.NotEqual(t, oldPID, newPID)

	out, err = run(newBin, "daemon", "status")
	require.NoError(t, err, out)
	assert.Contains(t, out, "v9.9.9-test")
}

func TestRestartWhenNotRunningStartsFresh(t *testing.T) {
	bin := buildDocbank(t)
	dir := t.TempDir()
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
