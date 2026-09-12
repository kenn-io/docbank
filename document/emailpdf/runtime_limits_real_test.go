package emailpdf

import (
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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
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
	spool := t.TempDir()
	r, err := NewRuntime(RuntimeConfig{Worker: worker, WorkerSHA256: workerHash, Chromium: filepath.Join(bundle, "chrome"), Bundle: bundle, BundleSHA256: bundleHash, Fonts: fonts, FontsSHA256: fontsHash, Version: "151.0.7922.34", Spool: spool})
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
	t.Run("worker-timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
		defer cancel()
		type result struct {
			pdf   []byte
			pages int64
			err   error
		}
		done := make(chan result, 1)
		started := time.Now()
		go func() {
			pdf, pages, err := r.Render(ctx, HTML{Bytes: []byte("<html><body>" + strings.Repeat("<div style='break-after:page'>Timeout qualification</div>", 1000) + "</body></html>")})
			done <- result{pdf, pages, err}
		}()
		// Suspend the real connected worker group. This forces a deadline
		// without relying on machine speed or weakening the production limit.
		var unit string
		deadline := time.Now().Add(10 * time.Second)
		for unit == "" && time.Now().Before(deadline) {
			paths, err := filepath.Glob(filepath.Join(spool, "email-pdf-*", "renderer-ready"))
			require.NoError(t, err)
			for _, path := range paths {
				pidBytes, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				pid, err := strconv.Atoi(string(pidBytes))
				if err != nil || pid <= 0 {
					continue
				}
				group, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
				if err != nil {
					continue
				}
				candidate := filepath.Base(strings.TrimSpace(string(group)))
				if strings.HasPrefix(candidate, "docbank-email-pdf-") && strings.HasSuffix(candidate, ".service") {
					unit = candidate
					break
				}
			}
			if unit == "" {
				time.Sleep(time.Millisecond)
			}
		}
		if unit == "" {
			cancel()
			<-done
			t.Fatal("real renderer readiness was not observed")
		}
		output, err := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "kill", "--signal=STOP", "--kill-whom=all", unit).CombinedOutput()
		if err != nil {
			cancel()
			<-done
			t.Fatalf("suspend exact real worker: %v: %s", err, output)
		}
		out := <-done
		require.Error(t, out.err)
		require.Empty(t, out.pdf)
		require.Zero(t, out.pages)
		require.GreaterOrEqual(t, time.Since(started), 55*time.Second)
		require.Less(t, time.Since(started), 75*time.Second)
		t.Logf("real suspended worker expired and drained after %s: %v", time.Since(started), out.err)
	})
	t.Run("chromium-memory-pressure", func(t *testing.T) {
		dir := t.TempDir()
		// A large but admitted DOM exercises real Chromium memory growth.
		// No fake renderer or reduced memory ceiling is used.
		html := "<html><body>" + strings.Repeat("<span>Synthetic</span>", 2_000_000) + "</body></html>"
		require.Less(t, len(html), maxHTMLBytes)
		require.NoError(t, r.writeInputs(dir, []byte(html)))
		unit := "docbank-email-pdf-" + uuid.NewString() + ".service"
		ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
		defer cancel()
		cmd := r.command(ctx, dir, unit)
		// Retain only the terminal unit's accounting for inspection. Every
		// execution/resource property and the real worker remain unchanged.
		cmd.Args = slices.DeleteFunc(cmd.Args, func(arg string) bool { return arg == "--collect" })
		t.Cleanup(func() { collectLimitUnit(t, unit) })
		var oomKills atomic.Int64
		observeCtx, stopObserver := context.WithCancel(ctx)
		observed := make(chan struct{})
		go func() {
			defer close(observed)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-observeCtx.Done():
					return
				case <-ticker.C:
					events, err := os.ReadFile(filepath.Join("/sys/fs/cgroup/system.slice", unit, "memory.events"))
					if err != nil {
						continue
					}
					for line := range strings.SplitSeq(string(events), "\n") {
						if value, ok := strings.CutPrefix(line, "oom_kill "); ok {
							count, err := strconv.ParseInt(value, 10, 64)
							if err == nil && count > oomKills.Load() {
								oomKills.Store(count)
							}
						}
					}
				}
			}
		}()
		err := cmd.Run()
		stopObserver()
		<-observed
		require.Error(t, err)
		accounting, showErr := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "show", unit, "--property=Result", "--property=MemoryPeak", "--property=MemoryMax", "--property=MemorySwapMax").Output()
		require.NoError(t, showErr)
		t.Logf("real Chromium memory accounting: %s", accounting)
		// systemd's OOMPolicy=continue may leave the browser host alive
		// until the fixed deadline. The cgroup event proves an actual OOM,
		// independently of that terminal result label.
		require.Positive(t, oomKills.Load(), "real Chromium must reach the enforced OOM boundary")
		t.Logf("observed actual cgroup oom_kill=%d", oomKills.Load())
		require.Contains(t, string(accounting), "MemoryMax=536870912")
		require.Contains(t, string(accounting), "MemorySwapMax=0")
		_, err = os.Stat(filepath.Join(dir, "message.pdf"))
		require.ErrorIs(t, err, os.ErrNotExist, "OOM must not produce a successful PDF")
	})
	t.Run("kernel-output-ceiling", func(t *testing.T) {
		dir := t.TempDir()
		unit := "docbank-email-pdf-" + uuid.NewString() + ".service"
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := r.command(ctx, dir, unit)
		// This is an explicit kernel-boundary control, not renderer proof:
		// keep the production properties and attempt an oversized sparse file.
		cmd.Args = append(cmd.Args[:len(cmd.Args)-5], "/usr/bin/truncate", "--size="+strconv.Itoa(MaxPDFBytes+1), filepath.Join(dir, "oversized-control"))
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
		unit := "docbank-email-pdf-" + uuid.NewString() + ".service"
		require.NoError(t, os.WriteFile(filepath.Join(dir, unitMarker), []byte(unit), 0600))
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		cmd := r.command(ctx, dir, unit)
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { collectLimitUnit(t, unit) })
		ready := false
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(filepath.Join(dir, "renderer-ready")); err == nil {
				ready = true
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !ready {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("real recovery worker never became ready")
		}
		stopErr := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "kill", "--signal=STOP", "--kill-whom=all", unit).Run()
		// Model loss of the owning launcher without politely stopping its
		// real descendant unit. Recovery, not Render's defer, must clean it.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		require.NoError(t, stopErr)
		require.NoError(t, RecoverStale(ctx, spool))
		_, err = os.Stat(dir)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	entries, err := os.ReadDir(spool)
	require.NoError(t, err)
	require.Empty(t, entries, "worker staging must be removed on success and limit rejection")
}

func collectLimitUnit(t *testing.T, unit string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "sudo", "-n", "systemctl", "stop", unit).Run()
	_ = exec.CommandContext(ctx, "sudo", "-n", "systemctl", "reset-failed", unit).Run()
	state, err := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "show", unit, "--property=LoadState", "--value").Output()
	require.NoError(t, err)
	require.Equal(t, "not-found", strings.TrimSpace(string(state)), "limit-control unit must be collected")
}
