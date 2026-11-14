package mcp

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/docbank/internal/filepublish"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

func reportArchiveFixture(t *testing.T) (report.Summary, []byte) {
	t.Helper()
	f := newNativeExportFixture(t)
	member := f.addFile(t, "alpha.txt", "alpha")
	summary, err := f.connection.CreateTermReport(t.Context(), report.Request{
		Version: 1, Timezone: "UTC", CoverageMode: "available_only",
		SelectedDocuments: &report.SelectedDocuments{Documents: []report.Identity{
			{NodeID: member.NodeID, VersionID: member.VersionID, SHA256: member.SHA256},
		}}, Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2020-01-01", End: "2100-01-01"}}},
	})
	require.NoError(t, err)
	stream, err := f.connection.OpenTermReport(t.Context(), summary.ID, "bundle")
	require.NoError(t, err)
	raw, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	return summary, raw
}

func reportArchiveHandler(t *testing.T, summary report.Summary, packet []byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected report write: %s", r.Method)
			return
		}
		switch r.URL.Path {
		case "/api/v1/search-exports/" + summary.ID:
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.MarshalWrite(w, summary))
		case "/api/v1/search-exports/" + summary.ID + "/bundle":
			w.Header().Set("Content-Length", strconv.FormatInt(summary.BundleBytes, 10))
			w.Header().Set("X-Docbank-Report-Sha256", summary.BundleSHA256)
			_, _ = w.Write(packet)
		default:
			t.Errorf("unexpected report request: %s", r.URL.Path)
		}
	}
}

func TestMCPReportDownloadPublication(t *testing.T) {
	summary, packet := reportArchiveFixture(t)
	daemon := httptest.NewServer(reportArchiveHandler(t, summary, packet))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true},
		exportTestLease(t, daemon.URL))
	destination := filepath.Join(t.TempDir(), "report.zip")
	args := map[string]any{"report_id": summary.ID, "destination_path": destination}
	output := exportCall(t, server, "download_report", args)
	require.Equal(t, "published", output["state"])
	require.Equal(t, false, output["cleanup_failed"])
	require.Equal(t, summary.BundleSHA256, output["sha256"])
	verification := objectField(t, output, "verification")
	require.Equal(t, true, verification["internally_consistent"])
	require.Equal(t, false, verification["source_verified"])
	saved, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, packet, saved)
	archive, err := zip.NewReader(bytes.NewReader(saved), int64(len(saved)))
	require.NoError(t, err)
	manifest, err := archive.Open("manifest.json")
	require.NoError(t, err)
	var counts struct {
		Counts []report.Counts `json:"counts"`
	}
	require.NoError(t, json.UnmarshalRead(manifest, &counts))
	require.NoError(t, manifest.Close())
	require.Equal(t, []report.Counts{{Hits: 1, HitsPlusFamily: 1, UniqueHits: 1,
		UniqueFamilies: 1, UniqueHitsPlusFamily: 1}}, counts.Counts)
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "download_report", "arguments": args,
	}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	saved, err = os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, packet, saved)
	args["overwrite"] = true
	require.Equal(t, "published", exportCall(t, server, "download_report", args)["state"])
}

func TestMCPReportDownloadDropsInterruptedConnection(t *testing.T) {
	summary, packet := reportArchiveFixture(t)
	interrupted := httptest.NewServer(reportArchiveHandler(t, summary, packet[:len(packet)/2]))
	defer interrupted.Close()
	restarted := httptest.NewServer(reportArchiveHandler(t, summary, packet))
	defer restarted.Close()
	var acquisitions atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		url := interrupted.URL
		if acquisitions.Add(1) > 1 {
			url = restarted.URL
		}
		connection := daemonconn.New(url, "synthetic-report-key")
		t.Cleanup(func() { require.NoError(t, connection.Close()) })
		return connection, nil
	}, func(c *daemonconn.Connection) error { return c.Close() })
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowReportWrites: true}, lease)
	directory := t.TempDir()
	destination := filepath.Join(directory, "report.zip")
	args := map[string]any{"report_id": summary.ID, "destination_path": destination}
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "download_report", "arguments": args,
	}))
	require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
	require.EqualValues(t, 1, acquisitions.Load(), "interrupted downloads are not replayed")
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Empty(t, entries)

	output := exportCall(t, server, "download_report", args)
	require.Equal(t, "published", output["state"])
	require.EqualValues(t, 2, acquisitions.Load(), "an explicit retry acquires the current daemon")
	saved, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, packet, saved)
}

func TestMCPReportDownloadAfterPublication(t *testing.T) {
	summary, packet := reportArchiveFixture(t)
	daemon := httptest.NewServer(reportArchiveHandler(t, summary, packet))
	defer daemon.Close()
	for _, mode := range []string{"destination race", "directory sync", "cleanup", "sync and cleanup"} {
		t.Run(mode, func(t *testing.T) {
			publish, cleanup := publishReportFile, cleanupReportStage
			t.Cleanup(func() { publishReportFile, cleanupReportStage = publish, cleanup })
			publishReportFile = func(stage, path string, overwrite bool) (bool, error) {
				if mode == "destination race" {
					require.NoError(t, os.WriteFile(path, []byte("concurrent file"), 0o600))
				}
				published, err := publish(stage, path, overwrite)
				if published && strings.Contains(mode, "sync") {
					return true, errors.New("synthetic directory sync failure")
				}
				return published, err
			}
			if strings.Contains(mode, "cleanup") {
				cleanupReportStage = func(stage *filepublish.Stage) error {
					return errors.Join(cleanup(stage), errors.New("synthetic cleanup failure"))
				}
			}
			var diagnostics bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true,
				Logger: slog.New(slog.NewTextHandler(&diagnostics, nil))}, exportTestLease(t, daemon.URL))
			destination := filepath.Join(t.TempDir(), "report.zip")
			output := exportCall(t, server, "download_report", map[string]any{
				"report_id": summary.ID, "destination_path": destination,
			})
			saved, err := os.ReadFile(destination)
			require.NoError(t, err)
			if mode == "destination race" {
				require.Equal(t, "concurrent file", string(saved))
				require.Equal(t, "report_local_io", output["code"])
				require.Contains(t, diagnostics.String(), "publish stage")
			} else {
				require.Equal(t, packet, saved)
				state := "published"
				if strings.Contains(mode, "sync") {
					state = "published_durability_unknown"
					require.Contains(t, diagnostics.String(), "synthetic directory sync failure")
				}
				require.Equal(t, state, output["state"])
				require.Equal(t, strings.Contains(mode, "cleanup"), output["cleanup_failed"])
				if strings.Contains(mode, "cleanup") {
					require.Contains(t, diagnostics.String(), "synthetic cleanup failure")
				}
			}
			encoded, err := json.Marshal(output)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "synthetic")
		})
	}
}

func TestMCPReportDownloadRejectsUnverifiedPackets(t *testing.T) {
	summary, packet := reportArchiveFixture(t)
	for _, mode := range []string{"corrupt", "summary mismatch", "needs review", "early local failure"} {
		t.Run(mode, func(t *testing.T) {
			advertised := summary
			body := bytes.Clone(packet)
			switch mode {
			case "corrupt":
				body[0] ^= 1
			case "summary mismatch":
				advertised.BundleSHA256 = strings.Repeat("b", 64)
			case "needs review":
				advertised.State = "needs_review"
				advertised.Counts = nil
				advertised.CSVBytes = 0
				advertised.BundleBytes = 0
			}
			serve := reportArchiveHandler(t, advertised, body)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "summary mismatch" && strings.HasSuffix(r.URL.Path, "/bundle") {
					reportArchiveHandler(t, summary, body)(w, r)
					return
				}
				serve(w, r)
			}))
			defer daemon.Close()
			dir := t.TempDir()
			if mode == "early local failure" {
				t.Setenv("DOCBANK_HOME", dir+string(os.PathSeparator)+"missing"+string(os.PathSeparator)+
					".."+string(os.PathSeparator)+"vault")
			}
			var diagnostics bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true,
				Logger: slog.New(slog.NewTextHandler(&diagnostics, nil))}, exportTestLease(t, daemon.URL))
			output := exportCall(t, server, "download_report", map[string]any{
				"report_id": summary.ID, "destination_path": filepath.Join(dir, "report.zip"),
			})
			want := "report_integrity"
			if mode == "needs review" {
				want = "date_review_required"
			}
			if mode == "early local failure" {
				want = "report_local_io"
				require.Contains(t, diagnostics.String(), "check destination")
				require.NotContains(t, output["message"], "stage")
			}
			require.Equal(t, want, output["code"])
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

type pausedReportBudget struct {
	report.Budget

	paused  *atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func (b *pausedReportBudget) Child() report.Budget {
	return &pausedReportBudget{b.Budget.Child(), b.paused, b.entered, b.resume}
}

func (b *pausedReportBudget) Reserve(ctx context.Context, size int64) (func(), error) {
	release, err := b.Budget.Reserve(ctx, size)
	if err == nil && b.paused.CompareAndSwap(false, true) {
		close(b.entered)
		select {
		case <-b.resume:
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		}
	}
	return release, err
}

func TestMCPReportDownloadSharesBudgetAndCancels(t *testing.T) {
	summary, packet := reportArchiveFixture(t)
	daemon := httptest.NewServer(reportArchiveHandler(t, summary, packet))
	defer daemon.Close()
	for _, mode := range []string{"overlap", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root := report.NewBudget(summary.BundleBytes*2 + 4096)
			defer func() { _ = root.Close() }()
			budget := &pausedReportBudget{Budget: root, paused: &atomic.Bool{},
				entered: make(chan struct{}), resume: make(chan struct{})}
			tools := &reportTools{lease: exportTestLease(t, daemon.URL), budget: budget,
				logger: slog.New(slog.DiscardHandler)}
			firstPath := filepath.Join(t.TempDir(), "first.zip")
			input, err := json.Marshal(reportDownloadInput{ReportID: summary.ID, DestinationPath: firstPath})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finished := make(chan error, 1)
			go func() { _, err := tools.download(ctx, input); finished <- err }()
			<-budget.entered
			require.Equal(t, summary.BundleBytes, root.Used())
			if mode == "cancel" {
				cancel()
				require.ErrorIs(t, <-finished, context.Canceled)
				_, err := os.Stat(firstPath)
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				secondPath := filepath.Join(t.TempDir(), "second.zip")
				other, err := json.Marshal(reportDownloadInput{ReportID: summary.ID, DestinationPath: secondPath})
				require.NoError(t, err)
				_, err = tools.download(t.Context(), other)
				require.ErrorIs(t, err, report.ErrReportLimit)
				require.Equal(t, summary.BundleBytes, root.Used(), "failed verification released its reservations")
				close(budget.resume)
				require.NoError(t, <-finished)
				output, err := tools.download(t.Context(), other)
				require.NoError(t, err)
				require.Equal(t, "published", output.State)
			}
			require.Zero(t, root.Used())
		})
	}
}
