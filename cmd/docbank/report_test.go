package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

func TestReportCoverageOutputShowsAvailableOnlyLimits(t *testing.T) {
	var output bytes.Buffer
	summary := report.Summary{
		Terms: []report.Term{{Number: 7, Expression: "synthetic alpha"}},
		Coverage: report.Coverage{Scoped: 10, Searchable: 8, MissingText: 2,
			IncompleteFamilies: 1, FallbackDates: 3, Warnings: []string{"synthetic coverage warning"}},
		RowCoverage: []report.Coverage{{Scoped: 10, Searchable: 7, MissingText: 2}},
	}
	require.NoError(t, writeReportCoverage(&output, summary))
	require.Contains(t, output.String(), "coverage: 10 scoped, 8 searchable, 2 missing text, 1 incomplete families, 3 fallback dates")
	require.Contains(t, output.String(), "counts exclude unavailable text or family evidence")
	require.Contains(t, output.String(), "term 7 coverage: 7 searchable")
	require.Contains(t, output.String(), "warning: synthetic coverage warning")
	output.Reset()
	require.NoError(t, writeReportCoverage(&output, report.Summary{State: "needs_review", UnresolvedDates: 2}))
	require.Contains(t, output.String(), "coverage pending date review")
	require.NotContains(t, output.String(), "0 scoped")
}

func TestReportArtifactDownloadDoesNotPublishCorruptOrDeniedBody(t *testing.T) {
	id := strings.Repeat("a", 48)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-owner" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"title":"Forbidden","status":403,"code":"forbidden"}`))
			return
		}
		body := []byte("synthetic-artifact")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("X-Docbank-Report-Sha256", strings.Repeat("0", 64))
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	for _, key := range []string{"synthetic-owner", "wrong-owner"} {
		t.Run(key, func(t *testing.T) {
			connection := daemonconn.New(server.URL, key)
			path := filepath.Join(t.TempDir(), "report.csv")
			err := downloadReportArtifactFile(t.Context(), connection, id, "csv", path, false)
			require.Error(t, err)
			_, statErr := os.Stat(path)
			require.ErrorIs(t, statErr, os.ErrNotExist)
			entries, err := os.ReadDir(filepath.Dir(path))
			require.NoError(t, err)
			require.Empty(t, entries, "failed downloads must remove private staging")
		})
	}
}

func TestReportBundleDownloadRejectsMatchingTransportHashForInvalidPacket(t *testing.T) {
	id := strings.Repeat("a", 48)
	body := []byte("synthetic invalid report bundle")
	sum := sha256.Sum256(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-owner" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("X-Docbank-Report-Sha256", hex.EncodeToString(sum[:]))
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "report.zip")
	err := downloadReportArtifactFile(t.Context(), daemonconn.New(server.URL, "synthetic-owner"),
		id, "bundle", path, false)
	require.ErrorIs(t, err, report.ErrInvalidPacket)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Empty(t, entries, "invalid bundles must remove private staging")
}

func TestReportDownloadSeparateInvocation(t *testing.T) {
	const child = "DOCBANK_TEST_REPORT_DOWNLOAD_CHILD"
	if os.Getenv(child) == "1" {
		rootCmd.SetArgs([]string{"search-export", "download", os.Getenv("DOCBANK_TEST_REPORT_ID"),
			"--format", os.Getenv("DOCBANK_TEST_REPORT_FORMAT"), "--output", os.Getenv("DOCBANK_TEST_REPORT_OUTPUT")})
		if err := rootCmd.ExecuteContext(context.Background()); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	home := setupVaultHome(t)
	source := writeSourceFile(t, "synthetic-report.txt", "synthetic alpha")
	_, err := runCLI(t, "add", source, "--dest", "/")
	require.NoError(t, err)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	input := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(input, encoded, 0o600))
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	// The report ID is the frozen handle returned by the create command.
	created, err := runCLI(t, "search-export", "create", "--input", input, "--output", filepath.Join(t.TempDir(), "second.zip"))
	require.NoError(t, err)
	id := regexp.MustCompile(`export ([0-9a-f]{48})`).FindStringSubmatch(created)[1]
	summary, err := connection.GetTermReport(t.Context(), id)
	require.NoError(t, err)
	for _, format := range []string{"csv", "bundle"} {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "download."+format)
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReportDownloadSeparateInvocation$") //nolint:gosec // os.Args[0] is this test executable; no shell or user command text.
			cmd.Env = append(os.Environ(), child+"=1", "DOCBANK_HOME="+home,
				"DOCBANK_TEST_REPORT_ID="+id, "DOCBANK_TEST_REPORT_FORMAT="+format,
				"DOCBANK_TEST_REPORT_OUTPUT="+path)
			stdout, err := cmd.Output()
			require.NoError(t, err)
			require.Empty(t, stdout)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			stream, err := connection.OpenTermReport(t.Context(), id, format)
			require.NoError(t, err)
			defer func() { _ = stream.Close() }()
			require.Equal(t, stream.SHA256, fmt.Sprintf("%x", sha256.Sum256(got)))
			wantSHA := summary.CSVSHA256
			if format == "bundle" {
				wantSHA = summary.BundleSHA256
			}
			require.Equal(t, wantSHA, stream.SHA256)
			_, err = runCLI(t, "search-export", "download", id, "--format", format, "--output", path)
			require.ErrorContains(t, err, "--overwrite")
		})
	}
}

func TestReportCLIUsesDaemonThenVerifiesPacketOffline(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "synthetic-alpha.txt", "synthetic alpha")
	_, err := runCLI(t, "add", source, "--dest", "/")
	require.NoError(t, err)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
		CoverageMode: "available_only", Terms: []report.Term{{Number: 1, Expression: "alpha",
			Syntax: "simple", Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	input := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(input, encoded, 0o600))
	packet := filepath.Join(t.TempDir(), "report.zip")
	output, err := runCLI(t, "search-export", "create", "--input", input, "--output", packet)
	require.NoError(t, err)
	require.Contains(t, output, "internally verified")
	_, err = runCLI(t, "search-export", "create", "--input", input, "--output", packet)
	require.ErrorContains(t, err, "--overwrite")

	// The next commands must work with no path to a running vault.
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "not-a-vault"))
	output, err = runCLI(t, "search-export", "verify", packet)
	require.NoError(t, err)
	require.Contains(t, output, "internally consistent: true; source verified: false")
	csv := filepath.Join(t.TempDir(), "hits.csv")
	_, err = runCLI(t, "search-export", "csv", packet, "--output", csv)
	require.NoError(t, err)
	content, err := os.ReadFile(csv)
	require.NoError(t, err)
	require.Contains(t, string(content), "Unique Families")
	require.Contains(t, string(content), "alpha")
}
