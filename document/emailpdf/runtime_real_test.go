package emailpdf

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Real qualification is explicit and always synthetic. Unit tests never
// substitute a fake child for Linux resource and descendant enforcement.
func TestRuntimeReal(t *testing.T) {
	bundle := os.Getenv("DOCBANK_EMAILPDF_TEST_BUNDLE")
	fonts := os.Getenv("DOCBANK_EMAILPDF_TEST_FONTS")
	if bundle == "" || fonts == "" {
		t.Skip("set DOCBANK_EMAILPDF_TEST_BUNDLE and DOCBANK_EMAILPDF_TEST_FONTS for isolated Chromium qualification")
	}
	bundleHash, err := TreeSHA256(bundle)
	require.NoError(t, err)
	fontsHash, err := TreeSHA256(fonts)
	require.NoError(t, err)
	spool := t.TempDir()
	worker := os.Getenv("DOCBANK_EMAILPDF_TEST_WORKER")
	workerHash, err := FileSHA256(worker)
	require.NoError(t, err)
	r, err := NewRuntime(RuntimeConfig{Worker: worker, WorkerSHA256: workerHash, Chromium: filepath.Join(bundle, "chrome"), Bundle: bundle, BundleSHA256: bundleHash, Fonts: fonts, FontsSHA256: fontsHash, Version: "151.0.7922.34", Spool: spool})
	require.NoError(t, err)
	t.Logf("runtime bundle=%s fonts=%s worker=%s", bundleHash, fontsHash, workerHash)
	for _, paper := range []string{"A4", "Letter"} {
		t.Run(paper, func(t *testing.T) {
			h, err := decodedHTML(t, "Subject: Unicode résumé\r\nDate: Mon, 01 Jan 2024 12:34:56 +0530\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<h1>Résumé 世界</h1><blockquote hidden>Full original quote</blockquote><img src='https://example.test/never-fetch'><p>"+strings.Repeat("LongURLabcdef", 300)+"</p>", paper)
			require.NoError(t, err)
			first, pages, err := r.Render(t.Context(), h)
			require.NoError(t, err)
			require.Positive(t, pages)
			second, again, err := r.Render(t.Context(), h)
			require.NoError(t, err)
			require.Equal(t, pages, again)
			require.Equal(t, first, second, "same pinned runtime must produce identical normalized bytes")
			path := filepath.Join(t.TempDir(), "synthetic.pdf")
			require.NoError(t, os.WriteFile(path, first, 0600))
			text, err := exec.CommandContext(t.Context(), "pdftotext", path, "-").Output()
			require.NoError(t, err)
			for _, want := range []string{"Résumé", "世界", "Full original quote", "+0530", "remote resource"} {
				require.Contains(t, string(text), want)
			}
			t.Logf("paper=%s pages=%d bytes=%d sha256=%x", paper, pages, len(first), sha256.Sum256(first))
		})
	}
	t.Run("descendant-cancellation", func(t *testing.T) {
		h := HTML{Bytes: []byte("<html><body>" + strings.Repeat("<div style='page-break-after:always'>Synthetic cancellation page</div>", 20000) + "</body></html>")}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ready := make(chan error, 1)
		go func() {
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					ready <- ctx.Err()
					return
				case <-ticker.C:
					paths, _ := filepath.Glob(filepath.Join(spool, "email-pdf-*", "renderer-ready"))
					for _, path := range paths {
						b, err := os.ReadFile(path)
						if err != nil {
							continue
						}
						pid, err := strconv.Atoi(string(b))
						if err != nil || pid < 1 {
							continue
						}
						comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
						if err != nil || !strings.Contains(string(comm), "chrome") {
							continue
						}
						cancel()
						ready <- nil
						return
					}
				}
			}
		}()
		_, _, err := r.Render(ctx, h)
		cancel()
		require.NoError(t, <-ready, "cancellation must follow real Chromium readiness")
		require.Error(t, err)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})
	entries, err := os.ReadDir(spool)
	require.NoError(t, err)
	require.Empty(t, entries, "all staging must be removed after real rendering and cancellation")
}
