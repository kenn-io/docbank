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
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/mcp"
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

func TestReportCSVDownloadRejectsInvalidCompanionWithMatchingTransportHashes(t *testing.T) {
	id := strings.Repeat("a", 48)
	csv := []byte("synthetic,standalone,csv\r\n")
	bundle := []byte("synthetic invalid companion packet")
	csvHash := sha256.Sum256(csv)
	bundleHash := sha256.Sum256(bundle)
	summary := report.Summary{
		ID: id, State: "complete", ObservedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
		Terms:     []report.Term{{Number: 1, Expression: "synthetic", Syntax: "simple"}},
		Counts:    []report.Counts{{Hits: 1}},
		CSVBytes:  int64(len(csv)), CSVSHA256: hex.EncodeToString(csvHash[:]),
		BundleBytes: int64(len(bundle)), BundleSHA256: hex.EncodeToString(bundleHash[:]),
	}
	var bundleRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-owner" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/v1/search-exports/" + id:
			w.Header().Set("Content-Type", "application/json")
			if err := json.MarshalWrite(w, summary); err != nil {
				t.Errorf("write synthetic summary: %v", err)
			}
		case "/api/v1/search-exports/" + id + "/csv":
			w.Header().Set("Content-Length", strconv.Itoa(len(csv)))
			w.Header().Set("X-Docbank-Report-Sha256", summary.CSVSHA256)
			_, _ = w.Write(csv)
		case "/api/v1/search-exports/" + id + "/bundle":
			bundleRequests++
			w.Header().Set("Content-Length", strconv.Itoa(len(bundle)))
			w.Header().Set("X-Docbank-Report-Sha256", summary.BundleSHA256)
			_, _ = w.Write(bundle)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "report.csv")
	err := downloadReportArtifactFile(t.Context(), daemonconn.New(server.URL, "synthetic-owner"),
		id, "csv", path, false)
	require.ErrorIs(t, err, report.ErrInvalidPacket)
	require.Equal(t, 1, bundleRequests)
	_, statErr := os.Stat(path)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	entries, readErr := os.ReadDir(filepath.Dir(path))
	require.NoError(t, readErr)
	require.Empty(t, entries, "failed CSV proof must remove the private companion")
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
	if os.Getenv(child) == "history" {
		rootCmd.SetArgs([]string{"search-export", "history", "--offset", "0", "--limit", "2"})
		if err := rootCmd.ExecuteContext(context.Background()); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(child) == "summary" {
		rootCmd.SetArgs([]string{"search-export", "show", os.Getenv("DOCBANK_TEST_REPORT_ID")})
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
	summaryCommand := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReportDownloadSeparateInvocation$") //nolint:gosec // This test executable is the subprocess; no shell or user command text.
	summaryCommand.Env = append(os.Environ(), child+"=summary", "DOCBANK_HOME="+home, "DOCBANK_TEST_REPORT_ID="+id)
	summaryOutput, err := summaryCommand.CombinedOutput()
	require.NoError(t, err, string(summaryOutput))
	var shown report.Summary
	require.NoError(t, json.Unmarshal(summaryOutput, &shown))
	require.Equal(t, summary.ID, shown.ID)
	require.Equal(t, summary.Counts, shown.Counts)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	mcpContext, stopMCP := context.WithCancel(t.Context())
	mcpDone := make(chan error, 1)
	go func() { mcpDone <- mcp.NewServer().Run(mcpContext, serverTransport) }()
	t.Cleanup(func() {
		stopMCP()
		<-mcpDone
	})
	mcpClient := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-report-client"}, nil)
	mcpSession, err := mcpClient.Connect(mcpContext, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mcpSession.Close() })
	mcpHistory, err := mcpSession.CallTool(mcpContext, &sdkmcp.CallToolParams{
		Name: "list_report_history", Arguments: map[string]any{"offset": 0, "limit": 1},
	})
	require.NoError(t, err)
	require.False(t, mcpHistory.IsError)
	mcpHistoryJSON, err := json.Marshal(mcpHistory.StructuredContent)
	require.NoError(t, err)
	var mcpPage struct {
		Items []struct {
			Summary report.Summary `json:"summary"`
		} `json:"items"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(mcpHistoryJSON, &mcpPage))
	require.Equal(t, 1, mcpPage.Total)
	require.Len(t, mcpPage.Items, 1)
	require.Equal(t, shown.ID, mcpPage.Items[0].Summary.ID)
	require.Equal(t, shown.Counts, mcpPage.Items[0].Summary.Counts)
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
	historyCommand := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReportDownloadSeparateInvocation$") //nolint:gosec // This test executable is the subprocess; no shell or user command text.
	historyCommand.Env = append(os.Environ(), child+"=history", "DOCBANK_HOME="+home)
	historyOutput, err := historyCommand.CombinedOutput()
	require.NoError(t, err, string(historyOutput))
	var history struct {
		Items []struct {
			Request report.Request `json:"request"`
			Summary report.Summary `json:"summary"`
		} `json:"items"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(historyOutput, &history))
	require.Equal(t, 1, history.Total)
	require.Len(t, history.Items, 1)
	require.Equal(t, id, history.Items[0].Summary.ID)
	require.Equal(t, request, history.Items[0].Request)
	_, err = runCLI(t, "search-export", "history", "--limit", "51")
	require.ErrorContains(t, err, "limit")
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
