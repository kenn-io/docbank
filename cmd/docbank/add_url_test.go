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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestAddURLImportsEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCBANK_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[server]\nidle_timeout = \"150ms\"\n"), 0o600))
	binary := buildDocbank(t)
	t.Cleanup(func() { _, _ = daemonconn.Stop(context.Background(), home) })
	content := "synthetic filing\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Eventually(t, func() bool {
			records, err := daemonconn.RuntimeStore(home).List()
			return err == nil && len(records) == 0
		}, 5*time.Second, 10*time.Millisecond, "daemon should idle out during the download")
		_, _ = io.WriteString(w, content)
	}))
	defer server.Close()
	requested := server.URL + "/filing.txt"
	command := exec.Command(binary, "add", requested, "--allow-http", "--dest", "/cases/filings", "--json")
	out, err := command.CombinedOutput()
	require.NoError(t, err, string(out))
	var report api.IngestReport
	require.NoError(t, json.Unmarshal(out, &report))
	assert.Equal(t, 1, report.Added)
	require.NotEmpty(t, report.IngestID)
	ingestID := report.IngestID
	evidence, err := runCLI(t, "provenance", "/cases/filings/filing.txt", "--json")
	require.NoError(t, err)
	var page api.ProvenancePage
	require.NoError(t, json.Unmarshal([]byte(evidence), &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, ingestID, page.Items[0].IngestID)
	require.NotNil(t, page.Items[0].URLFetch)
}

func TestFetchURLBoundsSchemesNamesAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, disposition, target, finalTarget, want, message string
		etag, lastModified                                    string
		declared, bytes, cap, status                          int
		chunked                                               bool
	}{
		{name: "boundary", target: "/files/report%20one.txt?q=1", bytes: 4, cap: 4, want: "report one.txt"},
		{name: "redirect evidence", target: "/download?id=7", finalTarget: "/files/filing.txt", bytes: 4, cap: 4, want: "filing.txt", etag: "W/\"edition-\xff\"", lastModified: "Wed, 21 Oct 2015 07:28:00 GMT"},
		{name: "disposition", disposition: `attachment; filename="server.txt"`, target: "/", bytes: 4, cap: 4, want: "server.txt"},
		{name: "newline path", target: "/report%0Aone.txt", bytes: 4, cap: 4, want: "download"},
		{name: "backslash path", target: "/report%5Cone.txt", bytes: 4, cap: 4, want: "download"},
		{name: "drive path", target: "/C:filing.txt", bytes: 4, cap: 4, want: "download"},
		{name: "drive disposition", disposition: `attachment; filename="a:b.txt"`, target: "/header-name.txt", bytes: 4, cap: 4, want: "header-name.txt"},
		{name: "chunked", chunked: true, bytes: 5, cap: 4, message: "download exceeds"},
		{name: "interrupted", declared: 5, bytes: 2, cap: 5, message: "downloading:"},
		{name: "HTTP error", status: http.StatusNotFound, cap: 4, message: "fetch returned HTTP 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			t.Setenv("TMP", tmp)
			t.Setenv("TEMP", tmp)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "identity", r.Header.Get("Accept-Encoding"))
				if tc.finalTarget != "" && r.URL.Path == "/download" {
					http.Redirect(w, r, tc.finalTarget, http.StatusFound)
					return
				}
				w.Header().Set("ETag", tc.etag)
				w.Header().Set("Last-Modified", tc.lastModified)
				w.Header().Set("Content-Disposition", tc.disposition)
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
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
				assert.Equal(t, tc.want, download.name)
				assert.Equal(t, server.URL+tc.target, download.fetch.RequestedURL)
				final := tc.target
				if tc.finalTarget != "" {
					final = tc.finalTarget
					assert.Equal(t, []byte(tc.etag), download.fetch.ETag)
					assert.Equal(t, []byte(tc.lastModified), download.fetch.LastModified)
					assert.NotEmpty(t, download.fetch.FetchedAt)
				}
				assert.Equal(t, server.URL+final, download.fetch.FinalURL)
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

func TestAddURLValidatesUsage(t *testing.T) {
	setupVaultHome(t)
	local := writeSourceFile(t, "local.txt", "local")
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"https://example.com/filing.txt", "--allow-http", "--max-bytes", "0"}, "--max-bytes must be between"},
		{[]string{"https://example.com/filing.txt", "local.txt", "--allow-http"}, "URL import requires one URL"},
		{[]string{local, "https://example.com/filing.txt", "--allow-http"}, "URL import requires one URL"},
		{[]string{"https://example.com/filing.txt", "--allow-http", "--preflight"}, "URL import requires one URL"},
		{[]string{"https://example.com/filing.txt", "--allow-http", "--replace"}, "URL import requires one URL"},
		{[]string{"https://example.com/filing.txt", "--allow-http", "--include", "*.pdf"}, "URL import requires one URL"},
		{[]string{"https://example.com/filing.txt", "--allow-http", "--progress", "typo"}, "invalid --progress"},
	} {
		_, err := runCLI(t, append([]string{"add"}, tc.args...)...)
		require.ErrorContains(t, err, tc.message)
	}
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
	setupFakeDaemon(t, http.NotFound)
	tmp, home := t.TempDir(), os.Getenv("DOCBANK_HOME")
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
}

func TestIsURLSourcePreservesLocalColonFile(t *testing.T) {
	assert.False(t, isURLSource("https:2026.txt"))
}
