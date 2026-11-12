package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
)

var docbankBuildDir string // Allocated on first build; TestMain removes it.
var docbankBuildMu sync.Mutex
var docbankBuilds = map[string]struct {
	bin string
	err error
}{}

// ensureDocbankBuildDir allocates the shared build directory on first use,
// so test processes that never build a binary leave no directory behind.
func ensureDocbankBuildDir() (err error) {
	if docbankBuildDir == "" {
		docbankBuildDir, err = os.MkdirTemp("", "docbank-test-binaries-")
	}
	return err
}

func buildDocbank(tb testing.TB) string {
	tb.Helper()
	return buildDocbankVersion(tb, "")
}

// buildDocbankVersion builds the binary, stamping internal/version.Version
// when v is non-empty (an unstamped build reports "dev").
func buildDocbankVersion(tb testing.TB, v string) string {
	tb.Helper()
	return buildDocbankWithWebOverlay(tb, v, false)
}

func buildDocbankWithWebOverlay(tb testing.TB, v string, webOverlay bool) string {
	tb.Helper()
	if testing.Short() {
		tb.Skip("integration test: builds the binary")
	}
	docbankBuildMu.Lock()
	defer docbankBuildMu.Unlock()
	key := fmt.Sprintf("%s:%t", v, webOverlay)
	build, ok := docbankBuilds[key]
	if !ok {
		build.bin, build.err = func() (string, error) {
			if err := ensureDocbankBuildDir(); err != nil {
				return "", err
			}
			name := fmt.Sprintf("docbank-%d", len(docbankBuilds))
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			bin := filepath.Join(docbankBuildDir, name)
			args := []string{"build", "-tags", "fts5"}
			if webOverlay {
				entry := filepath.Join(docbankBuildDir, "index.html")
				if err := os.WriteFile(entry, []byte("<!doctype html><html><head><title>Docbank</title></head><body></body></html>"), 0o600); err != nil {
					return "", err
				}
				webIndex, err := filepath.Abs(filepath.Join("..", "..", "internal", "web", "dist", "index.html"))
				if err != nil {
					return "", err
				}
				overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{webIndex: entry}})
				if err != nil {
					return "", err
				}
				overlayPath := filepath.Join(docbankBuildDir, "overlay.json")
				if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
					return "", err
				}
				args = append(args, "-overlay", overlayPath)
			}
			if v != "" {
				args = append(args, "-ldflags", "-X go.kenn.io/docbank/internal/version.Version="+v)
			}
			args = append(args, "-o", bin, "go.kenn.io/docbank/cmd/docbank")
			cmd := exec.CommandContext(tb.Context(), "go", args...)
			cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("build docbank: %w\n%s", err, out)
			}
			return bin, nil
		}()
		docbankBuilds[key] = build
	}
	require.NoError(tb, build.err)
	return build.bin
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
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// status/stop before any daemon: report, don't spawn.
	out, err := run("daemon", "status")
	require.Error(t, err) // exit 1 when not running
	assert.Contains(t, out, "not running")
	recs, lerr := daemonconn.RuntimeStore(dir).List()
	require.NoError(t, lerr)
	assert.Empty(t, recs, "status must not autostart")

	out, err = run("daemon", "start")
	require.NoError(t, err, out)
	t.Cleanup(func() { _, _ = daemonconn.Stop(context.Background(), dir) })
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
	_, _, found, err := daemonconn.Find(ctx, dir)
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
	cmd := exec.CommandContext(t.Context(), bin, "daemon", "start")
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
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		logs, err := filepath.Glob(filepath.Join(dir, "logs", "*.log"))
		if err != nil {
			t.Logf("listing daemon logs: %v", err)
			return
		}
		for _, path := range logs {
			data, err := os.ReadFile(path)
			t.Logf("daemon log %s (read error: %v):\n%s", filepath.Base(path), err, data)
		}
	})
	env := append(os.Environ(), "DOCBANK_HOME="+dir)

	run := func(bin string, args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run(oldBin, "daemon", "start")
	require.NoError(t, err, out)
	t.Cleanup(func() { _, _ = daemonconn.Stop(context.Background(), dir) })
	oldPID := parsePID(t, out)

	// A same-version start leaves the running daemon alone.
	out, err = run(oldBin, "daemon", "start")
	require.NoError(t, err, out)
	assert.Contains(t, out, "already running")
	assert.Equal(t, oldPID, parsePID(t, out))

	// Protocol 9 predates background-job status. Ensure must replace it before
	// the CLI requests /api/v1/jobs rather than sending the new request to a
	// same-version daemon that will return 404.
	recs, err := daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NotEmpty(t, recs[0].Metadata["protocol_version"])
	recs[0].Metadata["protocol_version"] = "9"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "jobs", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"items"`)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	jobsPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, oldPID, jobsPID)

	// A same-version daemon from before ingest preflight/exclusions existed
	// must be replaced before the CLI issues the new request. Otherwise
	// preflight returns 404 and a real ingest can silently ignore exclusions.
	src := filepath.Join(t.TempDir(), "preflight.txt")
	require.NoError(t, os.WriteFile(src, []byte("preview"), 0o600))
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NotEmpty(t, recs[0].Metadata["protocol_version"])
	recs[0].Metadata["protocol_version"] = "7"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)

	out, err = run(oldBin, "add", src, "--preflight")
	require.NoError(t, err, out)
	assert.Contains(t, out, "files: 1")
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	preflightPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, jobsPID, preflightPID)

	// Protocol 8 predates the ordinary ingest progress stream. Human-mode add
	// must replace it before requesting that endpoint rather than fail with a
	// 404 after the user has already selected a source tree.
	recs[0].Metadata["protocol_version"] = "8"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "add", src, "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "scan:")
	assert.Contains(t, out, "ingest:")
	assert.Contains(t, out, "added: 1")
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	ingestPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, preflightPID, ingestPID)

	// Protocol 10 predates stable content versions. Replace it before resolving
	// the new endpoint so a same-version stale daemon cannot return a misleading
	// node without current-version identity or a 404 for the listing.
	recs[0].Metadata["protocol_version"] = "10"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "versions", "list", "/inbox/preflight.txt")
	require.NoError(t, err, out)
	assert.Contains(t, out, "content_create")
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	versionsPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, ingestPID, versionsPID)

	// Protocol 11 allowed a POSIX filename with non-UTF-8 bytes to poison
	// metadata export. Replace it before any new add reaches that ingest path.
	boundarySrc := filepath.Join(t.TempDir(), "utf8-boundary.txt")
	require.NoError(t, os.WriteFile(boundarySrc, []byte("safe metadata"), 0o600))
	recs[0].Metadata["protocol_version"] = "11"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "add", boundarySrc, "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "added: 1")
	recs, err = daemonconn.RuntimeStore(dir).List()
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
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "put", replacementSrc, "/inbox/preflight.txt", "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "updated /inbox/preflight.txt")
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	putPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, metadataPID, putPID)

	// Protocol 13 predates content reversion. Replace it before the CLI sends
	// the mutation so an older same-version daemon cannot return 404 after the
	// operator has selected a historical source.
	out, err = run(oldBin, "document", "versions", "list", "/inbox/preflight.txt", "--json")
	require.NoError(t, err, out)
	var versionPage api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &versionPage))
	require.Len(t, versionPage.Items, 2)
	sourceVersionID := versionPage.Items[1].ID
	recs[0].Metadata["protocol_version"] = "13"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "revert", "/inbox/preflight.txt", sourceVersionID, "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"transition_kind":"content_revert"`)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	revertPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, putPID, revertPID)

	// Protocol 15 predates tag organization. Replace it before a tag command
	// reaches a same-version daemon that cannot honor the new authority.
	recs[0].Metadata["protocol_version"] = "15"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "tag", "create", "protocol-check", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"name":"protocol-check"`)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	tagPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, revertPID, tagPID)

	// Protocol 16 predates explicit version pruning. Replace it before a dry
	// run reaches a same-version daemon that would return 404 instead of a
	// trustworthy history inventory.
	recs[0].Metadata["protocol_version"] = "16"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "versions", "prune", "/inbox/preflight.txt", "--all-prior", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"checkpoint_required":true`)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	prunePID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, tagPID, prunePID)

	// Protocol 17 predates permanent audit enrollment and status. Replace it
	// before the CLI reaches a same-version daemon without the audit contract.
	recs[0].Metadata["protocol_version"] = "17"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "audit", "status", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"enabled": false`)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	auditPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, prunePID, auditPID)

	// Protocol 18 predates bounded audit-history reads. Replace it before the
	// CLI reaches a same-version daemon that cannot expose canonical events.
	recs[0].Metadata["protocol_version"] = "18"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "audit", "history", "/inbox/preflight.txt", "--json")
	require.Error(t, err)
	assert.Contains(t, out, "not enrolled in an audit scope")
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	historyPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, auditPID, historyPID)

	// Protocol 20 predates exact-prefix checks against externally recorded
	// audit evidence. Replace it before any expected evidence can be ignored.
	recs[0].Metadata["protocol_version"] = "20"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "audit", "verify", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"enabled": false`)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	verifyPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, historyPID, verifyPID)

	// Protocol 23 predates vault introspection. Replace it before the CLI asks
	// the daemon to identify the selected vault and summarize its contents.
	recs[0].Metadata["protocol_version"] = "23"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "vault", "info", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"vault_id"`)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	infoPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, verifyPID, infoPID)

	// Protocol 24 predates transactional batch moves. Replace it before the
	// CLI submits a plan that an older same-version daemon cannot recognize.
	batchPlan := filepath.Join(t.TempDir(), "batch-move.json")
	require.NoError(t, os.WriteFile(batchPlan, []byte(`{"moves":[{"source":"/inbox/preflight.txt","destination":"/inbox/protocol-batch.txt"}]}`), 0o600))
	recs[0].Metadata["protocol_version"] = "24"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "mv", "batch", batchPlan)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"/inbox/preflight.txt" -> "/inbox/protocol-batch.txt"`)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	batchPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, infoPID, batchPID)

	// Protocol 33 returned an internal storage-derived path when a trashed node
	// was inspected by ID. Replace it before stat can present that value as a
	// live virtual coordinate.
	out, err = run(oldBin, "document", "remove", "/inbox/protocol-batch.txt", "--json")
	require.NoError(t, err, out)
	var trashed api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &trashed))
	require.NotEmpty(t, trashed.TrashedAt)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NotEmpty(t, recs[0].Metadata["protocol_version"])
	recs[0].Metadata["protocol_version"] = "33"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "document", "stat", formatNodeSelector(trashed.ID), "--json")
	require.NoError(t, err, out)
	var inspected api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &inspected))
	assert.Equal(t, trashed.ID, inspected.ID)
	assert.Empty(t, inspected.Path)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	statPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, batchPID, statPID)

	// Protocol 34 predates effective watched-inbox inspection. Replace it
	// before the CLI calls the new route, even when no watches are configured.
	recs[0].Metadata["protocol_version"] = "34"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)
	out, err = run(oldBin, "watch", "list", "--json")
	require.NoError(t, err, out)
	var watches api.WatchedInboxList
	require.NoError(t, json.Unmarshal([]byte(out), &watches))
	assert.Empty(t, watches.Items)
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	watchPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, statPID, watchPID)

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
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NotEmpty(t, recs[0].Metadata["protocol_version"])
	recs[0].Metadata["protocol_version"] = "4"
	_, err = daemonconn.RuntimeStore(dir).Write(recs[0])
	require.NoError(t, err)

	out, err = run(oldBin, "backup", "create", "--repo", repoPath, "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "freeze:")
	assert.Contains(t, out, "created snapshot")
	recs, err = daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	protocolPID := strconv.Itoa(recs[0].PID)
	assert.NotEqual(t, watchPID, protocolPID)

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
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("daemon", "restart")
	require.NoError(t, err, out)
	t.Cleanup(func() { _, _ = daemonconn.Stop(context.Background(), dir) })
	assert.Contains(t, out, "started (was not running)")
}

func TestDaemonLifetimeTransitions(t *testing.T) {
	bin := buildDocbank(t)
	root := t.TempDir()
	run := func(bin string, args ...string) string {
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Env = append(os.Environ(), "DOCBANK_HOME="+root)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	t.Cleanup(func() { _, _ = daemonconn.Stop(context.WithoutCancel(t.Context()), root) })

	run(bin, "search", "missing")
	automaticPID := waitForDaemon(t, root).PID
	var status map[string]any
	run(bin, "daemon", "start")
	assert.Equal(t, automaticPID, waitForDaemon(t, root).PID)
	require.NoError(t, json.Unmarshal([]byte(run(bin, "daemon", "status", "--json")), &status))
	assert.Equal(t, false, status["explicit_start"])
	assert.Equal(t, "30m0s", status["idle_timeout"])

	run(bin, "daemon", "restart")
	assert.Contains(t, run(bin, "daemon", "status"), "Stays running until docbank daemon stop")
	require.NoError(t, json.Unmarshal([]byte(run(bin, "daemon", "status", "--json")), &status))
	assert.Equal(t, true, status["explicit_start"])
	assert.Equal(t, "0s", status["idle_timeout"])

	legacy := waitForDaemon(t, root)
	delete(legacy.Metadata, daemonconn.MetaExplicitStart)
	delete(legacy.Metadata, daemonconn.MetaIdleTimeout)
	_, err := daemonconn.RuntimeStore(root).Write(legacy)
	require.NoError(t, err)
	status = nil
	require.NoError(t, json.Unmarshal([]byte(run(bin, "daemon", "status", "--json")), &status))
	assert.Equal(t, true, status["running"])
	assert.NotContains(t, status, "explicit_start")
	assert.NotContains(t, status, "idle_timeout")
}

func startOutput(rec kitdaemon.RuntimeRecord) string {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printEnsured(cmd, daemonconn.EnsureResult{Record: rec})
	return out.String()
}

func TestStartOutputReflectsDaemonLifetimeState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		lifetime string
		advice   bool
	}{
		{"legacy", nil, "Lifetime unavailable", true},
		{"automatic", map[string]string{daemonconn.MetaExplicitStart: "false", daemonconn.MetaIdleTimeout: "30m0s"}, "Exits after 30m idle (started automatically)", true},
		{"idle_off", map[string]string{daemonconn.MetaExplicitStart: "false", daemonconn.MetaIdleTimeout: "0s"}, "Idle exit is off", true},
		{"foreground", map[string]string{daemonconn.MetaExplicitStart: "false"}, "", false},
		// The explicit request survives later config changes and replacements.
		{"explicit", map[string]string{daemonconn.MetaExplicitStart: "true", daemonconn.MetaIdleTimeout: "0s"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := startOutput(kitdaemon.RuntimeRecord{Metadata: tc.metadata})
			if tc.lifetime == "" {
				assert.NotContains(t, out, "idle")
				assert.NotContains(t, out, "Lifetime")
				assert.NotContains(t, out, "Stays running")
			} else {
				assert.Contains(t, out, tc.lifetime)
			}
			if tc.advice {
				assert.Contains(t, out, "Run docbank daemon restart to keep it running.")
			} else {
				assert.NotContains(t, out, "Run docbank daemon restart to keep it running.")
			}
		})
	}
}

func TestWebRestartKeepsAutomaticLifetime(t *testing.T) {
	bin := buildDocbankWithWebOverlay(t, "", true)
	root := t.TempDir()
	t.Setenv("DOCBANK_LOCK_DIR", t.TempDir())
	env := append(os.Environ(), "DOCBANK_HOME="+root, "DOCBANK_TELEMETRY_ENABLED=0", "BROWSER=/bin/false")
	t.Cleanup(func() { _, _ = daemonconn.Stop(context.WithoutCancel(t.Context()), root) })
	launchWeb := func() *url.URL {
		cmd := exec.CommandContext(t.Context(), bin, "web", "--no-browser")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		launch, err := url.Parse(strings.TrimSpace(string(out)))
		require.NoError(t, err)
		return launch
	}
	launch := launchWeb()
	fragment, err := url.ParseQuery(launch.Fragment)
	require.NoError(t, err)
	token := fragment.Get("web_session")
	require.NotEmpty(t, token)
	old := waitForDaemon(t, root)
	require.False(t, daemonconn.Explicit(old))
	watcher, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Close() })
	require.NoError(t, watcher.Add(root))
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split host and port: %w", err)
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, launch.Scheme+"://"+launch.Host+"/api/v1/processing/local/restart", nil)
	require.NoError(t, err)
	request.Header.Set("Origin", launch.Scheme+"://"+launch.Host)
	request.Header.Set(api.WebSessionHeader, token)
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	require.NoError(t, response.Body.Close())
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			logs, _ := filepath.Glob(filepath.Join(root, "logs", "*.log"))
			for _, path := range logs {
				data, err := os.ReadFile(path)
				t.Logf("daemon log %s (read error: %v):\n%s", filepath.Base(path), err, data)
			}
			t.Fatalf("replacement daemon did not appear: %v", ctx.Err())
		case err := <-watcher.Errors:
			require.NoError(t, err)
		case <-watcher.Events:
			records, err := daemonconn.RuntimeStore(root).List()
			require.NoError(t, err)
			for _, rec := range records {
				if rec.PID != old.PID {
					require.False(t, daemonconn.Explicit(rec))
					return
				}
			}
		}
	}
}

func TestWebRestartLeavesStoppedOrReplacedDaemonAlone(t *testing.T) {
	bin := buildDocbank(t)
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOCBANK_LOCK_DIR", t.TempDir())
			t.Setenv("DOCBANK_HOME", root)
			t.Setenv("DOCBANK_TELEMETRY_ENABLED", "0")
			t.Cleanup(func() { _, _ = daemonconn.Stop(context.WithoutCancel(t.Context()), root) })
			start := exec.CommandContext(t.Context(), bin, "daemon", "start")
			out, err := start.CombinedOutput()
			require.NoError(t, err, string(out))
			old := waitForDaemon(t, root)
			lock, err := (home.Layout{Root: root}).TryLockLaunch()
			require.NoError(t, err)
			defer func() { _ = lock.Release() }()
			helper := exec.CommandContext(t.Context(), bin, "daemon", "restart", "--from-web")
			helper.Env = append(os.Environ(), "DOCBANK_WEB_RESTART_TOKEN="+old.Metadata["shutdown_token"])
			require.NoError(t, helper.Start())
			_, err = daemonconn.Stop(t.Context(), root)
			require.NoError(t, err)
			var replacement kitdaemon.RuntimeRecord
			if replace {
				process := exec.CommandContext(t.Context(), bin, "daemon", "run")
				stderr, err := process.StderrPipe()
				require.NoError(t, err)
				require.NoError(t, process.Start())
				t.Cleanup(func() { _, _ = daemonconn.Stop(context.WithoutCancel(t.Context()), root); _ = process.Wait() })
				ready := make(chan struct{})
				go func() {
					scanner := bufio.NewScanner(stderr)
					for scanner.Scan() {
						if strings.Contains(scanner.Text(), "docbank daemon listening") {
							close(ready)
						}
					}
				}()
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("replacement daemon did not start")
				}
				replacement = waitForDaemon(t, root)
			}
			require.NoError(t, lock.Release())
			require.NoError(t, helper.Wait())
			current, _, found, err := daemonconn.Find(t.Context(), root)
			require.NoError(t, err)
			require.Equal(t, replace, found)
			if replace {
				assert.Equal(t, replacement.PID, current.PID)
			}
		})
	}
}
