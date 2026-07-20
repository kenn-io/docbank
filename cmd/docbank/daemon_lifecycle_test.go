package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
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
	name := "docbank"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
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
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, exitBusy, exitErr.ExitCode(), string(out))
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

	// Protocol 9 predates background-job status. Ensure must replace it before
	// the CLI requests /api/v1/jobs rather than sending the new request to a
	// same-version daemon that will return 404.
	recs, err := client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "23", recs[0].Metadata["protocol_version"])
	recs[0].Metadata["protocol_version"] = "9"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "jobs", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"items"`)
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	jobsPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, oldPID, jobsPID)

	// A same-version daemon from before ingest preflight/exclusions existed
	// must be replaced before the CLI issues the new request. Otherwise
	// preflight returns 404 and a real ingest can silently ignore exclusions.
	src := filepath.Join(t.TempDir(), "preflight.txt")
	require.NoError(t, os.WriteFile(src, []byte("preview"), 0o600))
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "23", recs[0].Metadata["protocol_version"])
	recs[0].Metadata["protocol_version"] = "7"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)

	out, err = run(oldBin, "add", src, "--preflight")
	require.NoError(t, err, out)
	assert.Contains(t, out, "files: 1")
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	preflightPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, jobsPID, preflightPID)

	// Protocol 8 predates the ordinary ingest progress stream. Human-mode add
	// must replace it before requesting that endpoint rather than fail with a
	// 404 after the user has already selected a source tree.
	recs[0].Metadata["protocol_version"] = "8"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "add", src, "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "scan:")
	assert.Contains(t, out, "ingest:")
	assert.Contains(t, out, "added: 1")
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	ingestPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, preflightPID, ingestPID)

	// Protocol 10 predates stable content versions. Replace it before resolving
	// the new endpoint so a same-version stale daemon cannot return a misleading
	// node without current-version identity or a 404 for the listing.
	recs[0].Metadata["protocol_version"] = "10"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "versions", "list", "/inbox/preflight.txt")
	require.NoError(t, err, out)
	assert.Contains(t, out, "content_create")
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	versionsPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, ingestPID, versionsPID)

	// Protocol 11 allowed a POSIX filename with non-UTF-8 bytes to poison
	// metadata export. Replace it before any new add reaches that ingest path.
	boundarySrc := filepath.Join(t.TempDir(), "utf8-boundary.txt")
	require.NoError(t, os.WriteFile(boundarySrc, []byte("safe metadata"), 0o600))
	recs[0].Metadata["protocol_version"] = "11"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "add", boundarySrc, "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "added: 1")
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	metadataPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, versionsPID, metadataPID)

	// Protocol 12 predates versioned content replacement. Replace it before
	// put starts its two-pass transfer so the CLI cannot hash a large source
	// only to receive a 404 from a same-version stale daemon.
	replacementSrc := filepath.Join(t.TempDir(), "replacement.txt")
	require.NoError(t, os.WriteFile(replacementSrc, []byte("replacement content"), 0o600))
	recs[0].Metadata["protocol_version"] = "12"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "put", replacementSrc, "/inbox/preflight.txt", "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "updated /inbox/preflight.txt")
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	putPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, metadataPID, putPID)

	// Protocol 13 predates content reversion. Replace it before the CLI sends
	// the mutation so an older same-version daemon cannot return 404 after the
	// operator has selected a historical source.
	out, err = run(oldBin, "versions", "list", "/inbox/preflight.txt", "--json")
	require.NoError(t, err, out)
	var versionPage api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &versionPage))
	require.Len(t, versionPage.Items, 2)
	sourceVersionID := versionPage.Items[1].ID
	recs[0].Metadata["protocol_version"] = "13"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "revert", "/inbox/preflight.txt", sourceVersionID, "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"transition_kind":"content_revert"`)
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	revertPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, putPID, revertPID)

	// Protocol 15 predates tag organization. Replace it before a tag command
	// reaches a same-version daemon that cannot honor the new authority.
	recs[0].Metadata["protocol_version"] = "15"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "tag", "create", "protocol-check", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"name":"protocol-check"`)
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	tagPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, revertPID, tagPID)

	// Protocol 16 predates explicit version pruning. Replace it before a dry
	// run reaches a same-version daemon that would return 404 instead of a
	// trustworthy history inventory.
	recs[0].Metadata["protocol_version"] = "16"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "versions", "prune", "/inbox/preflight.txt", "--all-prior", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"checkpoint_required":true`)
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	prunePID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, tagPID, prunePID)

	// Protocol 17 predates permanent audit enrollment and status. Replace it
	// before the CLI reaches a same-version daemon without the audit contract.
	recs[0].Metadata["protocol_version"] = "17"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "audit", "status", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"enabled": false`)
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	auditPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, prunePID, auditPID)

	// Protocol 18 predates bounded audit-history reads. Replace it before the
	// CLI reaches a same-version daemon that cannot expose canonical events.
	recs[0].Metadata["protocol_version"] = "18"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "audit", "history", "/inbox/preflight.txt", "--json")
	require.Error(t, err)
	assert.Contains(t, out, "not enrolled in an audit scope")
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	historyPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, auditPID, historyPID)

	// Protocol 20 predates exact-prefix checks against externally recorded
	// audit evidence. Replace it before any expected evidence can be ignored.
	recs[0].Metadata["protocol_version"] = "20"
	_, err = client.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "audit", "verify", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"enabled": false`)
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	verifyPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, historyPID, verifyPID)

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
	recs, err = client.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "23", recs[0].Metadata["protocol_version"])
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
	assert.NotEqual(t, verifyPID, protocolPID)

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
