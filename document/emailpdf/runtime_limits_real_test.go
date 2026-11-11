package emailpdf

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"
	"uuid"
)

// These opt-in checks use the pinned production worker, not a PDF stub.
func TestRuntimeRealLimits(t *testing.T) {
	bundle := os.Getenv("DOCBANK_EMAILPDF_TEST_BUNDLE")
	fonts := os.Getenv("DOCBANK_EMAILPDF_TEST_FONTS")
	worker := os.Getenv("DOCBANK_EMAILPDF_TEST_WORKER")
	if bundle == "" || fonts == "" || worker == "" {
		t.Skip("set the explicit email PDF runtime qualification paths")
	}
	bundleHash, err := TreeSHA256(bundle)
	require.NoError(t, err)
	fontsHash, err := TreeSHA256(fonts)
	require.NoError(t, err)
	workerHash, err := FileSHA256(worker)
	require.NoError(t, err)
	bubblewrap, err := exec.LookPath("bwrap")
	require.NoError(t, err)
	bubblewrapHash, err := FileSHA256(bubblewrap)
	require.NoError(t, err)
	spool := t.TempDir()
	r, err := NewRuntime(RuntimeConfig{Worker: worker, WorkerSHA256: workerHash, Bubblewrap: bubblewrap, BubblewrapSHA256: bubblewrapHash, Chromium: filepath.Join(bundle, "chrome"), Bundle: bundle, BundleSHA256: bundleHash, Fonts: fonts, FontsSHA256: fontsHash, Version: "151.0.7922.34", Spool: spool})
	require.NoError(t, err)
	t.Logf("bundle=%s fonts=%s worker=%s", bundleHash, fontsHash, workerHash)
	t.Run("plain-text-content", func(t *testing.T) {
		html, err := decodedHTML(t, "Subject: Synthetic plain message\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlain résumé 世界\r\n    indented source line\r\n> Full plain quote\r\n"+strings.Repeat("LongURLabcdef", 150)+"\r\nFinal plain marker", "Letter")
		require.NoError(t, err)
		pdf, pages, err := r.Render(t.Context(), html)
		require.NoError(t, err)
		require.Positive(t, pages)
		path := filepath.Join(t.TempDir(), "plain.pdf")
		require.NoError(t, os.WriteFile(path, pdf, 0600))
		text, err := exec.CommandContext(t.Context(), "pdftotext", "-layout", path, "-").Output()
		require.NoError(t, err)
		for _, want := range []string{"Plain résumé", "世界", "    indented source line", "> Full plain quote", "Final plain marker"} {
			require.Contains(t, string(text), want)
		}
		require.Equal(t, 150, strings.Count(strings.ReplaceAll(string(text), "\n", ""), "LongURLabcdef"), "wrapping must preserve the full long line")
	})
	t.Run("network-attempt", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		request.Header.Set("User-Agent", "OpenAI File Downloader, XaiImageApiFetch/1.0")
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.EqualValues(t, 1, requests.Load(), "observer must be reachable outside worker isolation")
		requests.Store(0)
		// Deliberately bypass sanitization to test the renderer's independent
		// resource boundary against a real reachable observer.
		html := "<html><body>Offline boundary<img src='" + server.URL + "/image'><link rel='stylesheet' href='" + server.URL + "/style'><iframe src='" + server.URL + "/frame'></iframe></body></html>"
		pdf, pages, err := r.Render(t.Context(), HTML{Bytes: []byte(html)})
		require.NoError(t, err)
		require.NotEmpty(t, pdf)
		require.Positive(t, pages)
		require.Zero(t, requests.Load(), "Chromium must not reach the observer")
	})
	t.Run("page-limit", func(t *testing.T) {
		html := "<html><head><style>@page{size:A4;margin:12mm}.page{break-after:page}</style></head><body>" + strings.Repeat("<div class='page'>Synthetic bounded page</div>", MaxPDFPages+1) + "</body></html>"
		pdf, pages, err := r.Render(context.Background(), HTML{Bytes: []byte(html)})
		require.ErrorContains(t, err, "page", "real over-limit PDF must be refused by page verification")
		require.Empty(t, pdf)
		require.Zero(t, pages)
	})
	t.Run("suspended-worker-cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		waitCtx, stopWaiting := context.WithCancel(ctx)
		defer stopWaiting()
		type result struct {
			pdf   []byte
			pages int64
			err   error
		}
		done := make(chan result, 1)
		go func() {
			pdf, pages, err := r.Render(ctx, HTML{Bytes: []byte("<html><body>" + strings.Repeat("<div style='break-after:page'>Timeout qualification</div>", 1000) + "</body></html>")})
			done <- result{pdf, pages, err}
			stopWaiting()
		}()
		// Suspend the real connected worker group so it can never finish on
		// its own, independently of machine speed.
		unit, err := waitForReadyRenderer(waitCtx, spool)
		if err != nil {
			cancel()
			<-done
			t.Fatalf("real renderer readiness was not observed: %v", err)
		}
		output, err := exec.CommandContext(ctx, "systemctl", "--user", "kill", "--signal=STOP", "--kill-whom=all", unit).CombinedOutput()
		if err != nil {
			cancel()
			<-done
			t.Fatalf("suspend exact real worker: %v: %s", err, output)
		}
		// Ending the caller's context reaches the worker the same way Render's
		// own 60-second deadline does: through the systemd-run client and the
		// unit stop that follows.
		cancel()
		out := <-done
		require.Error(t, out.err)
		require.Empty(t, out.pdf)
		require.Zero(t, out.pages)
		t.Logf("real suspended worker stopped and drained: %v", out.err)
	})
	t.Run("chromium-memory-pressure", func(t *testing.T) {
		dir := t.TempDir()
		// A large but admitted DOM exercises real Chromium memory growth.
		// No fake renderer or reduced memory ceiling is used.
		html := "<html><body>" + strings.Repeat("<span>Synthetic</span>", 2_000_000) + "</body></html>"
		require.Less(t, len(html), maxHTMLBytes)
		require.NoError(t, r.writeInputs(dir, []byte(html)))
		unit := "docbank-email-pdf-" + uuid.New().String() + ".service"
		slice := "docbanktest" + strings.ReplaceAll(uuid.New().String(), "-", "") + ".slice"
		anchor := "docbank-email-pdf-anchor-" + uuid.New().String() + ".service"
		ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
		defer cancel()
		t.Cleanup(func() { collectLimitUnit(t, anchor) })
		// Keep this test's slice alive even if systemd removes the worker's
		// cgroup immediately after its last process exits.
		output, err := exec.CommandContext(ctx, "systemd-run", "--user", "--quiet", "--unit="+anchor, "--slice="+slice, "/usr/bin/sleep", "infinity").CombinedOutput()
		require.NoError(t, err, "start test slice anchor: %s", output)
		group, err := exec.CommandContext(ctx, "systemctl", "--user", "show", anchor, "--property=ControlGroup", "--value").Output()
		require.NoError(t, err)
		groupPath := strings.TrimSpace(string(group))
		require.NotEmpty(t, groupPath)
		require.Equal(t, slice, filepath.Base(filepath.Dir(groupPath)))
		events := filepath.Join("/sys/fs/cgroup", filepath.Dir(groupPath), "memory.events")
		cmd := r.command(ctx, dir, unit, "/runtime/worker", r.workerArgs()...)
		var diagnostics bytes.Buffer
		cmd.Stderr = &diagnostics
		cmd.Args = slices.Insert(cmd.Args, 1, "--slice="+slice)
		// Retain only the terminal unit's accounting for inspection. Every
		// execution/resource property and the real worker remain unchanged.
		cmd.Args = slices.DeleteFunc(cmd.Args, func(arg string) bool { return arg == "--collect" })
		t.Cleanup(func() { collectLimitUnit(t, unit) })
		// The slice contains only this worker and the idle anchor. Its
		// hierarchical counter survives removal of the worker's cgroup.
		watcher, err := fsnotify.NewWatcher()
		require.NoError(t, err)
		defer func() { _ = watcher.Close() }()
		require.NoError(t, watcher.Add(events))
		oomKillsBefore := oomKillCount(t, events)
		finished := make(chan error, 1)
		go func() { finished <- cmd.Run() }()
		for running := true; running; {
			select {
			case err = <-finished:
				running = false
			case watchErr := <-watcher.Errors:
				require.NoError(t, watchErr)
			case <-watcher.Events:
				if oomKillCount(t, events) == oomKillsBefore {
					continue
				}
				// systemd's OOMPolicy=continue may leave the browser host alive
				// until the unit's fixed deadline. End the unit once the kernel
				// has reported the OOM kill instead of waiting for that deadline.
				if killErr := exec.CommandContext(ctx, "systemctl", "--user", "kill", "--signal=KILL", "--kill-whom=all", unit).Run(); killErr != nil {
					t.Logf("kill unit after OOM (it may already have exited): %v", killErr)
				}
				err = <-finished
				running = false
			}
		}
		oomKills := oomKillCount(t, events) - oomKillsBefore
		require.Error(t, err)
		if oomKills == 0 {
			t.Logf("worker exited before an OOM event: %s", diagnostics.String())
		}
		anchorState, stateErr := exec.CommandContext(ctx, "systemctl", "--user", "show", anchor, "--property=ActiveState", "--value").Output()
		require.NoError(t, stateErr)
		require.Equal(t, "active", strings.TrimSpace(string(anchorState)), "the idle slice anchor must not be the OOM victim")
		accounting, showErr := exec.CommandContext(ctx, "systemctl", "--user", "show", unit, "--property=Result", "--property=MemoryPeak", "--property=MemoryMax", "--property=MemorySwapMax").Output()
		require.NoError(t, showErr)
		t.Logf("real Chromium memory accounting: %s", accounting)
		// The cgroup event proves an actual OOM, independently of the
		// terminal result label.
		require.Positive(t, oomKills, "real Chromium must reach the enforced OOM boundary")
		t.Logf("observed actual cgroup oom_kill=%d", oomKills)
		require.Contains(t, string(accounting), "MemoryMax=536870912")
		require.Contains(t, string(accounting), "MemorySwapMax=0")
		_, err = os.Stat(filepath.Join(dir, "message.pdf"))
		require.ErrorIs(t, err, os.ErrNotExist, "OOM must not produce a successful PDF")
	})
	t.Run("kernel-output-ceiling", func(t *testing.T) {
		dir := t.TempDir()
		unit := "docbank-email-pdf-" + uuid.New().String() + ".service"
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		// Keep production limits and attempt an oversized sparse file.
		cmd := r.command(ctx, dir, unit, "/usr/bin/truncate", "--size="+strconv.Itoa(MaxPDFBytes+1), "/work/oversized-control")
		t.Cleanup(func() { collectLimitUnit(t, unit) })
		require.Error(t, cmd.Run(), "production RLIMIT_FSIZE must reject the oversized file")
		info, err := os.Stat(filepath.Join(dir, "oversized-control"))
		require.NoError(t, err)
		require.LessOrEqual(t, info.Size(), int64(MaxPDFBytes))
	})
	t.Run("restart-recovery", func(t *testing.T) {
		dir, err := os.MkdirTemp(spool, "email-pdf-") //nolint:usetesting // Recovery must find the production-owned prefix under this exact spool.
		require.NoError(t, err)
		require.NoError(t, r.writeInputs(dir, []byte("<html><body>"+strings.Repeat("<div style='break-after:page'>Synthetic recovery page</div>", 1000)+"</body></html>")))
		unit := "docbank-email-pdf-" + uuid.New().String() + ".service"
		require.NoError(t, os.WriteFile(filepath.Join(dir, unitMarker), []byte(unit), 0600))
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		cmd := r.command(ctx, dir, unit, "/runtime/worker", r.workerArgs()...)
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { collectLimitUnit(t, unit) })
		waitCtx, stopWaiting := context.WithCancel(ctx)
		defer stopWaiting()
		exited := make(chan error, 1)
		go func() {
			exited <- cmd.Wait()
			stopWaiting()
		}()
		ready, err := waitForReadyRenderer(waitCtx, spool)
		if err != nil || ready != unit {
			_ = cmd.Process.Kill()
			t.Fatalf("real recovery worker never became ready: unit=%q err=%v; worker exited with %v", ready, err, <-exited)
		}
		stopErr := exec.CommandContext(ctx, "systemctl", "--user", "kill", "--signal=STOP", "--kill-whom=all", unit).Run()
		// Model loss of the owning launcher without politely stopping its
		// real descendant unit. Recovery, not Render's defer, must clean it.
		_ = cmd.Process.Kill()
		t.Logf("killed recovery launcher exited with %v", <-exited)
		require.NoError(t, stopErr)
		require.NoError(t, RecoverStale(ctx, spool))
		_, err = os.Stat(dir)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	entries, err := os.ReadDir(spool)
	require.NoError(t, err)
	require.Empty(t, entries, "worker staging must be removed on success and limit rejection")
}

func oomKillCount(t *testing.T, eventsPath string) int64 {
	t.Helper()
	events, err := os.ReadFile(eventsPath)
	require.NoError(t, err)
	for line := range strings.SplitSeq(string(events), "\n") {
		if value, ok := strings.CutPrefix(line, "oom_kill "); ok {
			count, err := strconv.ParseInt(value, 10, 64)
			require.NoError(t, err)
			return count
		}
	}
	require.FailNow(t, "memory.events has no oom_kill count", "%s", eventsPath)
	return 0
}

func collectLimitUnit(t *testing.T, unit string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "systemctl", "--user", "stop", unit).Run()
	_ = exec.CommandContext(ctx, "systemctl", "--user", "reset-failed", unit).Run()
	state, err := exec.CommandContext(ctx, "systemctl", "--user", "show", unit, "--property=LoadState", "--value").Output()
	require.NoError(t, err)
	require.Equal(t, "not-found", strings.TrimSpace(string(state)), "limit-control unit must be collected")
}
