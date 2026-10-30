package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestAddURLImportsEvidence(t *testing.T) {
	setupVaultHome(t)
	content := "synthetic filing\n"
	etag := "W/\"edition-\xff\""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			http.Redirect(w, r, "/files/filing.txt", http.StatusFound)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Disposition", `attachment; filename="filing.txt"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		_, _ = io.WriteString(w, content)
	}))
	defer server.Close()
	requested := server.URL + "/download?id=7"
	out, err := runCLI(t, "add", requested, "--allow-http", "--dest", "/cases/filings", "--json")
	require.NoError(t, err, out)
	var report api.IngestReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.Added)
	require.NotEmpty(t, report.IngestID)
	ingestID := report.IngestID
	evidence, err := runCLI(t, "provenance", "/cases/filings/filing.txt", "--json")
	require.NoError(t, err)
	var page api.ProvenancePage
	require.NoError(t, json.Unmarshal([]byte(evidence), &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, ingestID, page.Items[0].IngestID)
	fetch := page.Items[0].URLFetch
	require.NotNil(t, fetch)
	assert.Equal(t, requested, fetch.RequestedURL)
	assert.Equal(t, server.URL+"/files/filing.txt", fetch.FinalURL)
	assert.Equal(t, []byte(etag), fetch.ETag)
	assert.Equal(t, []byte("Wed, 21 Oct 2015 07:28:00 GMT"), fetch.LastModified)
	assert.NotEmpty(t, fetch.FetchedAt)
	assert.Equal(t, requested, page.Items[0].OriginalPath)
	human, err := runCLI(t, "provenance", "/cases/filings/filing.txt")
	require.NoError(t, err)
	assert.Contains(t, human, "Requested URL:")
	assert.NotContains(t, human, "Source:")
	assert.Contains(t, human, strconv.Quote(string(fetch.LastModified)))
	assert.Contains(t, human, strconv.Quote(etag))
}

func TestFetchURLBoundsSchemesNamesAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, disposition, target, want, message string
		declared, bytes, cap                     int
		chunked                                  bool
	}{
		{name: "boundary", target: "/files/report%20one.txt?q=1", bytes: 4, cap: 4, want: "report one.txt"},
		{name: "disposition", disposition: `attachment; filename="server.txt"`, target: "/", bytes: 4, cap: 4, want: "server.txt"},
		{name: "fallback", disposition: `attachment; filename="../bad"`, target: "/", bytes: 4, cap: 4, want: "download"},
		{name: "newline path", target: "/report%0Aone.txt", bytes: 4, cap: 4, want: "download"},
		{name: "newline disposition", disposition: `attachment; filename*=UTF-8''report%0Done%0A.txt`, target: "/header-name.txt", bytes: 4, cap: 4, want: "header-name.txt"},
		{name: "chunked", chunked: true, bytes: 5, cap: 4, message: "download exceeds"},
		{name: "interrupted", declared: 5, bytes: 2, cap: 5, message: "downloading:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			t.Setenv("TMP", tmp)
			t.Setenv("TEMP", tmp)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "identity", r.Header.Get("Accept-Encoding"))
				w.Header().Set("Content-Disposition", tc.disposition)
				if tc.declared > 0 {
					w.Header().Set("Content-Length", strconv.Itoa(tc.declared))
				}
				if tc.chunked {
					assert.NoError(t, http.NewResponseController(w).Flush())
				}
				_, _ = io.WriteString(w, strings.Repeat("x", tc.bytes))
			}))
			defer server.Close()
			download, err := fetchURL(t.Context(), *server.Client(), server.URL+tc.target, false, int64(tc.cap))
			if tc.message != "" {
				require.ErrorContains(t, err, tc.message)
				assert.Nil(t, download)
			} else {
				require.NoError(t, err)
				assert.EqualValues(t, tc.bytes, download.size)
				assert.Empty(t, download.fetch.ETag)
				assert.Empty(t, download.fetch.LastModified)
				assert.Equal(t, tc.want, download.name)
				assert.Equal(t, server.URL+tc.target, download.fetch.RequestedURL)
				assert.Equal(t, server.URL+tc.target, download.fetch.FinalURL)
				require.NoError(t, download.file.Close())
				require.NoError(t, os.Remove(download.file.Name()))
			}
			files, err := filepath.Glob(filepath.Join(tmp, "docbank-url-*"))
			require.NoError(t, err)
			assert.Empty(t, files)
		})
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer plain.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL, http.StatusFound) }))
	defer redirect.Close()
	for _, raw := range []string{plain.URL, redirect.URL} {
		_, err := fetchURL(t.Context(), *redirect.Client(), raw, false, 4)
		require.ErrorContains(t, err, "--allow-http")
		download, err := fetchURL(t.Context(), *redirect.Client(), raw, true, 4)
		require.NoError(t, err)
		_ = download.file.Close()
		_ = os.Remove(download.file.Name())
	}
}

func TestAddURLFailuresPublishNoDocuments(t *testing.T) {
	setupVaultHome(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	local := writeSourceFile(t, "local.txt", "local")
	for _, tc := range []struct {
		args        []string
		message     string
		beforeFetch bool
	}{
		{[]string{server.URL + "/missing", "--allow-http"}, "fetch returned HTTP 404", false},
		{[]string{server.URL, "--allow-http", "--max-bytes", "0"}, "--max-bytes must be between", true},
		{[]string{server.URL, "local.txt", "--allow-http"}, "URL import requires one URL", true},
		{[]string{local, server.URL, "--allow-http"}, "URL import requires one URL", true},
		{[]string{server.URL, "--allow-http", "--preflight"}, "URL import requires one URL", true},
		{[]string{server.URL, "--allow-http", "--replace"}, "URL import requires one URL", true},
		{[]string{server.URL, "--allow-http", "--include", "*.pdf"}, "URL import requires one URL", true},
		{[]string{server.URL, "--allow-http", "--progress", "typo"}, "invalid --progress", true},
	} {
		requests.Store(0)
		_, err := runCLI(t, append([]string{"add"}, tc.args...)...)
		require.ErrorContains(t, err, tc.message)
		if tc.beforeFetch {
			assert.Zero(t, requests.Load())
		}
	}
	out, err := runCLI(t, "tree", "/")
	require.NoError(t, err)
	assert.NotContains(t, out, "inbox")
}

func TestAddURLInterruptRemovesSpool(t *testing.T) {
	if raw := os.Getenv("DOCBANK_TEST_URL_INTERRUPT"); raw != "" {
		rootCmd.SetArgs([]string{"add", raw, "--allow-http"})
		if err := rootCmd.Execute(); !errors.Is(err, context.Canceled) {
			os.Exit(1)
		}
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("os.Process.Signal cannot deliver os.Interrupt on Windows")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "prefix")
		assert.NoError(t, http.NewResponseController(w).Flush())
		<-r.Context().Done()
	}))
	defer server.Close()
	executable, err := os.Executable()
	require.NoError(t, err)
	tmp, home := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestAddURLInterruptRemovesSpool$")
	child.Env = append(os.Environ(), "DOCBANK_TEST_URL_INTERRUPT="+server.URL, "DOCBANK_HOME="+home, "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill() })
	require.Eventually(t, func() bool { files, _ := filepath.Glob(filepath.Join(tmp, "docbank-url-*")); return len(files) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, child.Process.Signal(os.Interrupt))
	require.NoError(t, child.Wait())
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, entries)
	entries, err = os.ReadDir(home)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestIsURLSourcePreservesLocalColonFile(t *testing.T) {
	assert.False(t, isURLSource("https:2026.txt"))
}
