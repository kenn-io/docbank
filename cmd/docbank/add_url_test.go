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

func TestAddURLImportsEvidenceAndSkips(t *testing.T) {
	setupVaultHome(t)
	content := "synthetic filing\n"
	etag := "W/\"edition-\xff\""
	var filename, payload atomic.Pointer[string]
	filename.Store(new("filing.txt"))
	payload.Store(new(content))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("X-Api-Key"))
		assert.Equal(t, "identity", r.Header.Get("Accept-Encoding"))
		if r.URL.Path == "/download" {
			http.Redirect(w, r, "/files/filing.txt", http.StatusFound)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Disposition", `attachment; filename="`+*filename.Load()+`"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		_, _ = io.WriteString(w, *payload.Load())
	}))
	defer server.Close()
	requested := server.URL + "/download?id=7"
	out, err := runCLI(t, "add", requested, "--allow-http", "--dest", "/cases/filings", "--json")
	require.NoError(t, err, out)
	var report api.IngestReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.Added)
	out, err = runCLI(t, "cat", "/cases/filings/filing.txt")
	require.NoError(t, err)
	assert.Equal(t, content, out)
	_, err = runCLI(t, "mv", "/cases/filings/filing.txt", "/cases/filings/reviewed.txt")
	require.NoError(t, err)
	before, err := runCLI(t, "stat", "/cases/filings/reviewed.txt", "--json")
	require.NoError(t, err)
	evidence, err := runCLI(t, "provenance", "/cases/filings/reviewed.txt", "--json")
	require.NoError(t, err)
	var page api.ProvenancePage
	require.NoError(t, json.Unmarshal([]byte(evidence), &page))
	require.Len(t, page.Items, 1)
	fetch := page.Items[0].URLFetch
	require.NotNil(t, fetch)
	assert.Equal(t, requested, fetch.RequestedURL)
	assert.Equal(t, server.URL+"/files/filing.txt", fetch.FinalURL)
	assert.Equal(t, []byte(etag), fetch.ETag)
	assert.Equal(t, []byte("Wed, 21 Oct 2015 07:28:00 GMT"), fetch.LastModified)
	assert.NotEmpty(t, fetch.FetchedAt)
	assert.Equal(t, requested, page.Items[0].OriginalPath)
	human, err := runCLI(t, "provenance", "/cases/filings/reviewed.txt")
	require.NoError(t, err)
	assert.Contains(t, human, "Requested URL:")
	assert.Contains(t, human, strconv.Quote(string(fetch.LastModified)))
	assert.Contains(t, human, strconv.Quote(etag))
	filename.Store(new("changed-name.txt"))
	out, err = runCLI(t, "add", requested, "--allow-http", "--dest", "/cases/filings", "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.Skipped)
	after, err := runCLI(t, "stat", "/cases/filings/reviewed.txt", "--json")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	after, err = runCLI(t, "provenance", "/cases/filings/reviewed.txt", "--json")
	require.NoError(t, err)
	assert.JSONEq(t, evidence, after)

	filename.Store(new("filing.txt"))
	for _, raw := range []string{server.URL + "/download?id=8", server.URL + "/download?id=9"} {
		out, err = runCLI(t, "add", raw, "--allow-http", "--dest", "/cases/filings", "--json")
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal([]byte(out), &report))
		assert.Equal(t, 1, report.Added)
	}
	filename.Store(new("filing (2).txt"))
	out, err = runCLI(t, "add", server.URL+"/download?id=10", "--allow-http", "--dest", "/cases/filings", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.Added)
	_, err = runCLI(t, "stat", "/cases/filings/filing (2) (2).txt")
	require.NoError(t, err)
	local := writeSourceFile(t, "filing.txt", content)
	out, err = runCLI(t, "add", local, "--dest", "/cases/filings", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.Added)
	filename.Store(new("revised.txt"))
	payload.Store(new("new edition"))
	out, err = runCLI(t, "add", requested, "--allow-http", "--dest", "/cases/filings", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.Added)
	out, err = runCLI(t, "cat", "/cases/filings/revised.txt")
	require.NoError(t, err)
	assert.Equal(t, "new edition", out)
}

func TestFetchURLBoundsSchemesNamesAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, disposition, target string
		declared, bytes, cap      int
		chunked, fail             bool
	}{
		{name: "boundary", target: "/files/report%20one.txt?q=1", bytes: 4, cap: 4},
		{name: "disposition", disposition: `attachment; filename="server.txt"`, target: "/", bytes: 4, cap: 4},
		{name: "fallback", disposition: `attachment; filename="../bad"`, target: "/", bytes: 4, cap: 4},
		{name: "chunked", chunked: true, bytes: 5, cap: 4, fail: true},
		{name: "interrupted", declared: 5, bytes: 2, cap: 5, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			t.Setenv("TMP", tmp)
			t.Setenv("TEMP", tmp)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			if tc.fail {
				require.Error(t, err)
				assert.Nil(t, download)
			} else {
				require.NoError(t, err)
				assert.EqualValues(t, tc.bytes, download.size)
				assert.Empty(t, download.fetch.ETag)
				assert.Empty(t, download.fetch.LastModified)
				want := "report one.txt"
				if tc.name == "disposition" {
					want = "server.txt"
				}
				if tc.name == "fallback" {
					want = "download"
				}
				assert.Equal(t, want, download.name)
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := fetchURL(ctx, http.Client{}, plain.URL, true, 4)
	require.ErrorIs(t, err, context.Canceled)
}

func TestAddURLFailuresPublishNoDocuments(t *testing.T) {
	setupVaultHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "five!")
	}))
	defer server.Close()
	for _, args := range [][]string{
		{server.URL}, {server.URL, "--allow-http", "--max-bytes", "4"}, {server.URL + "/missing", "--allow-http"},
		{server.URL, "local.txt"}, {writeSourceFile(t, "local.txt", "local"), server.URL}, {"ftp://example.com/file.txt"}, {server.URL, "--allow-http", "--max-bytes", "0"}, {server.URL, "--preflight"}, {server.URL, "--replace"}, {server.URL, "--include", "*.pdf"},
	} {
		_, err := runCLI(t, append([]string{"add"}, args...)...)
		require.Error(t, err)
	}
	out, err := runCLI(t, "tree", "/")
	require.NoError(t, err)
	assert.NotContains(t, out, "download")
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
