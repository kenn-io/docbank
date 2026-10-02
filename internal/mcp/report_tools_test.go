package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/reporting"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPReportOptIn(t *testing.T) {
	for _, options := range []ServerOptions{{}, {AllowProcessing: true}, {AllowPackageWrites: true},
		{AllowPhotoEdits: true}, {AllowExportWrites: true}, {AllowReportWrites: true},
		{AllowProcessing: true, AllowPackageWrites: true, AllowPhotoEdits: true, AllowExportWrites: true}} {
		var calls atomic.Int32
		lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
			calls.Add(1)
			return nil, errDaemonUnavailable
		}, func(*daemonconn.Connection) error { return nil })
		server := newServerWithOptionsAndDaemon(testImplementation(), options, lease)
		for _, transport := range []string{"stdio", "http"} {
			listed := decodeResult(t, exportExchange(t, server, transport, "tools/list", nil))
			names := listedToolNames(t, listed)
			for _, name := range []string{"get_report_summary", "get_report_dates"} {
				require.Contains(t, names, name)
				result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
					"name": name, "arguments": map[string]any{"report_id": strings.Repeat("a", 48)},
				}))
				require.Equal(t, "daemon_unavailable", objectField(t, result, "structuredContent")["code"])
			}
			tools := listedToolsByName(t, listed)
			for _, name := range []string{"create_report", "revise_report", "download_report"} {
				if options.AllowReportWrites {
					require.Contains(t, names, name)
					annotations := objectField(t, tools[name], "annotations")
					require.Equal(t, false, annotations["readOnlyHint"])
					require.Equal(t, false, annotations["idempotentHint"])
					require.Equal(t, name == "download_report", annotations["destructiveHint"])
				} else {
					require.NotContains(t, names, name)
					before := calls.Load()
					raw := exportExchange(t, server, transport, "tools/call", map[string]any{
						"name": name, "arguments": map[string]any{},
					})
					require.NotZero(t, decodeWireError(t, raw).Code)
					require.Equal(t, before, calls.Load())
				}
			}
			if options.AllowReportWrites {
				result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
					"name": "create_report", "arguments": map[string]any{"request": reportArguments()},
				}))
				require.Equal(t, "daemon_unavailable", objectField(t, result, "structuredContent")["code"])
			}
		}
	}
}

func reportArguments() map[string]any {
	return map[string]any{"version": 1, "timezone": "UTC",
		"selected_documents": map[string]any{"documents": []report.Identity{{
			NodeID: 7, VersionID: testVersionID, SHA256: strings.Repeat("a", 64),
		}}}, "terms": []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}}}}
}

func TestMCPReportRequestBoundary(t *testing.T) {
	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request report.Request
		assert.NoError(t, json.UnmarshalRead(r.Body, &request))
		assert.Equal(t, "strict", request.CoverageMode)
		assert.False(t, request.AllDocuments)
		assert.Len(t, request.SelectedDocuments.Documents, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":409,"code":"report_selection_changed","title":"Conflict"}`))
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true},
		exportTestLease(t, daemon.URL))
	for _, change := range []func(map[string]any){
		func(r map[string]any) { r["selected_documents"] = nil },
		func(r map[string]any) { r["unexpected"] = true },
		func(r map[string]any) { r["all_documents"] = true },
		func(r map[string]any) { r["collection_ids"] = []string{} },
		func(r map[string]any) { r["date_choices"] = []any{} },
		func(r map[string]any) { r["timezone"] = "Invalid/Zone" },
		func(r map[string]any) { r["source_timezone"] = strings.Repeat("é", 65) },
		func(r map[string]any) { r["terms"] = nil },
		func(r map[string]any) {
			r["selected_documents"] = map[string]any{"documents": []report.Identity{
				{NodeID: 7, VersionID: testVersionID, SHA256: strings.Repeat("a", 64)},
				{NodeID: 7, VersionID: testVersionID, SHA256: strings.Repeat("a", 64)},
			}}
		},
		func(r map[string]any) {
			r["selected_documents"] = map[string]any{"documents": make([]report.Identity, 1001)}
		},
		func(r map[string]any) {
			r["selected_documents"] = map[string]any{"documents": []report.Identity{
				{NodeID: 7, VersionID: "bad", SHA256: strings.Repeat("a", 64)},
			}}
		},
	} {
		args := reportArguments()
		change(args)
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": "create_report", "arguments": map[string]any{"request": args},
		}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
	for _, args := range []map[string]any{
		{"report_id": "invalid"},
		{"report_id": strings.Repeat("a", 48), "cursor": strings.Repeat("é", 2050)},
		{"report_id": strings.Repeat("a", 48), "limit": 0},
		{"report_id": strings.Repeat("a", 48), "max_bytes": 65536},
	} {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": "get_report_dates", "arguments": args,
		}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
	choice := report.DateChoice{Document: report.Identity{NodeID: 7, VersionID: testVersionID,
		SHA256: strings.Repeat("a", 64)}, CandidateID: strings.Repeat("b", 64),
		EvidenceSHA256: strings.Repeat("c", 64), Reason: " ", Action: "select"}
	for _, reason := range []string{" ", strings.Repeat("é", 2049)} {
		choice.Reason = reason
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": "revise_report", "arguments": map[string]any{
				"report_id": strings.Repeat("a", 48), "choices": []report.DateChoice{choice},
			},
		}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
	require.Zero(t, calls.Load())
	output := exportCall(t, server, "create_report", map[string]any{"request": reportArguments()})
	require.Equal(t, "report_selection_changed", output["code"])
	require.EqualValues(t, 1, calls.Load())
}

func TestMCPReportWriteDoesNotReplay(t *testing.T) {
	for _, operation := range []string{"create_report", "revise_report"} {
		for _, failure := range []string{"broken", "malformed", "invalid summary", "wrong parent", "invalid receipt", "known conflict"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				var calls atomic.Int32
				daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					w.Header().Set("Content-Type", "application/json")
					switch failure {
					case "broken":
						panic(http.ErrAbortHandler)
					case "malformed":
						_, _ = w.Write([]byte(`{`))
						return
					case "known conflict":
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"status":409,"title":"Conflict","code":"stale_evidence"}`))
						return
					}
					summary := report.Summary{ID: strings.Repeat("b", 48), State: "needs_review",
						ParentID: strings.Repeat("a", 48), ObservedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
						Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
							Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}}}}
					switch failure {
					case "invalid summary":
						summary.ID = "invalid"
					case "wrong parent":
						summary.ParentID = strings.Repeat("c", 48)
					case "invalid receipt":
						summary.UnresolvedDates = -1
					}
					_ = json.MarshalWrite(w, summary)
				}))
				defer daemon.Close()
				server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true},
					exportTestLease(t, daemon.URL))
				args := map[string]any{"request": reportArguments()}
				if operation == "revise_report" {
					args = map[string]any{"report_id": strings.Repeat("a", 48),
						"choices": []report.DateChoice{{Document: report.Identity{NodeID: 7, VersionID: testVersionID,
							SHA256: strings.Repeat("a", 64)}, CandidateID: strings.Repeat("b", 64), EvidenceSHA256: strings.Repeat("c", 64),
							Reason: "Reviewed source", Action: "select"}}}
				}
				output := exportCall(t, server, operation, args)
				if failure == "known conflict" {
					require.Equal(t, "stale_evidence", output["code"])
				} else if failure == "wrong parent" && operation == "create_report" {
					require.Equal(t, "needs_review", output["state"])
				} else {
					require.Equal(t, "report_outcome_unknown", output["code"], "%+v", output)
				}
				require.EqualValues(t, 1, calls.Load())
			})
		}
	}
}

func TestMCPReportDateWireBudget(t *testing.T) {
	f := newNativeExportFixture(t)
	var identities []report.Identity
	for i := range 3 {
		var content strings.Builder
		for j := range 200 {
			fmt.Fprintf(&content, "%s Document dated 2024-05-%02d. %s\n",
				strings.Repeat("\"\\\t", 90), j%2+1, strings.Repeat("\"\\\t", 90))
		}
		member := f.addFile(t, fmt.Sprintf("evidence-%d.txt", i), content.String())
		require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
			BlobHash: member.SHA256, Extractor: "synthetic-native", ExtractorVersion: 1,
			Status: store.ExtractionOK, Text: content.String(),
		}))
		identities = append(identities, report.Identity{NodeID: member.NodeID, VersionID: member.VersionID, SHA256: member.SHA256})
	}
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true}, f.lease)
	args := reportArguments()
	args["selected_documents"] = report.SelectedDocuments{Documents: identities}
	args["coverage_mode"] = "available_only"
	receipt := exportCall(t, server, "create_report", map[string]any{"request": args})
	require.Equal(t, "needs_review", receipt["state"], "%+v", receipt)
	require.NotContains(t, receipt, "bundle_bytes")
	require.NotContains(t, receipt, "bundle_sha256")
	id, ok := receipt["report_id"].(string)
	require.True(t, ok)
	var expected []report.DateCandidate
	request := report.DatePageRequest{}
	for {
		page, err := f.connection.TermReportDates(t.Context(), id, request)
		require.NoError(t, err)
		for _, member := range page.Members {
			expected = append(expected, member.Candidates...)
		}
		if page.NextCursor == "" {
			break
		}
		request.Cursor = page.NextCursor
	}
	require.Len(t, expected, 603)
	for _, transport := range []string{"stdio", "http"} {
		var got []report.DateCandidate
		cursor := ""
		pages := 0
		for {
			raw := exportExchange(t, server, transport, "tools/call", map[string]any{
				"name": "get_report_dates", "arguments": map[string]any{"report_id": id, "cursor": cursor},
			})
			require.LessOrEqual(t, len(raw), 1<<20)
			result := decodeResult(t, raw)
			output := objectField(t, result, "structuredContent")
			require.NotContains(t, output, "code", "%+v", output)
			encoded, err := json.Marshal(output["page"])
			require.NoError(t, err)
			require.LessOrEqual(t, len(encoded), 256<<10)
			var page report.DatePage
			require.NoError(t, json.Unmarshal(encoded, &page))
			require.NotEmpty(t, page.Members)
			for _, member := range page.Members {
				got = append(got, member.Candidates...)
			}
			pages++
			require.Less(t, pages, 20)
			if page.NextCursor == "" {
				break
			}
			require.NotEqual(t, cursor, page.NextCursor)
			cursor = page.NextCursor
		}
		require.Greater(t, pages, 1)
		require.Equal(t, expected, got)
	}
}

// This source retains real store evidence and supplies a large observation warning set.
// The cache still builds and verifies its real report artifacts.
type warningReportSource struct{ *store.Store }

func (s warningReportSource) MaterializeTermReportFrame(ctx context.Context, request report.Request,
	selection report.CoverageSelection, budget, textBudget report.Budget) (report.Frame, error) {
	frame, err := s.Store.MaterializeTermReportFrame(ctx, request, selection, budget, textBudget)
	for i := range 20000 {
		frame.Coverage.Warnings = append(frame.Coverage.Warnings,
			fmt.Sprintf("Synthetic family relation %d has incomplete retained evidence for this observation.", i))
	}
	return frame, err
}

func TestMCPReportLargeSummaryKeepsHandle(t *testing.T) {
	f := newNativeExportFixture(t)
	member := f.addFile(t, "alpha.txt", "alpha")
	budget := report.NewBudget(report.DefaultBudgetBytes)
	defer func() { _ = budget.Close() }()
	cache := reporting.NewCache(time.Now, budget)
	defer cache.InvalidateAll()
	service := &reporting.Service{Source: warningReportSource{f.catalog}}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/search-exports"), "/")
		if r.URL.Path == "/api/v1/search-exports" {
			var request report.Request
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			summary, err := cache.Create(r.Context(), "master", service, request)
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, summary))
		} else if strings.HasSuffix(r.URL.Path, "/dates") {
			var request report.DatePageRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			assert.Equal(t, 256<<10, request.MaxBytes)
			page, err := cache.Dates(r.Context(), "master", parts[1], request)
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, page))
		} else if strings.HasSuffix(r.URL.Path, "/bundle") {
			stream, size, digest, err := cache.Acquire(r.Context(), "master", parts[1], "bundle")
			assert.NoError(t, err)
			defer func() { _ = stream.Close() }()
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("X-Docbank-Report-Sha256", digest)
			_, err = io.Copy(w, stream)
			assert.NoError(t, err)
		} else {
			summary, err := cache.Summary("master", parts[1])
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, summary))
		}
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true},
		exportTestLease(t, daemon.URL))
	args := reportArguments()
	args["selected_documents"] = report.SelectedDocuments{Documents: []report.Identity{
		{NodeID: member.NodeID, VersionID: member.VersionID, SHA256: member.SHA256},
	}}
	args["coverage_mode"] = "available_only"
	receipt := exportCall(t, server, "create_report", map[string]any{"request": args})
	require.Equal(t, "complete", receipt["state"], "%+v", receipt)
	require.NotContains(t, receipt, "summary")
	id, ok := receipt["report_id"].(string)
	require.True(t, ok)
	output := exportCall(t, server, "get_report_summary", map[string]any{"report_id": id})
	require.Equal(t, "report_limit", output["code"])
	dates := exportCall(t, server, "get_report_dates", map[string]any{"report_id": id})
	require.Contains(t, dates, "page")
	output = exportCall(t, server, "download_report", map[string]any{
		"report_id": id, "destination_path": filepath.Join(t.TempDir(), "report.zip"),
	})
	require.Equal(t, "published", output["state"])
	require.Equal(t, true, objectField(t, output, "verification")["internally_consistent"])
}

func TestMCPReportResultMetadataLimit(t *testing.T) {
	f := newNativeExportFixture(t)
	member := f.addFile(t, "alpha.txt", "alpha")
	request := report.Request{Version: 1, SelectedDocuments: &report.SelectedDocuments{
		Documents: []report.Identity{{NodeID: member.NodeID, VersionID: member.VersionID, SHA256: member.SHA256}}},
		Timezone: "UTC", CoverageMode: "available_only", Terms: []report.Term{{Number: 1, Expression: "alpha",
			Syntax: "simple", Dates: report.DateRange{Start: "2020-01-01", End: "2100-01-01"}}}}
	summary, err := f.connection.CreateTermReport(t.Context(), request)
	require.NoError(t, err)
	implementation := testImplementation()
	implementation.Name = strings.Repeat("x", 1<<20)
	server := newServerWithOptionsAndDaemon(implementation, ServerOptions{AllowReportWrites: true}, f.lease)
	result := exportCall(t, server, "get_report_summary", map[string]any{"report_id": summary.ID})
	require.Equal(t, "report_limit", result["code"])
	result = exportCall(t, server, "create_report", map[string]any{"request": request})
	require.Equal(t, "report_outcome_unknown", result["code"])
}

func TestMCPReportCanceledWriteDoesNotReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
	}))
	defer daemon.Close()
	tools := &reportTools{lease: exportTestLease(t, daemon.URL), logger: slog.New(slog.DiscardHandler)}
	_, output := createReportSchemas()
	handler := tools.handler(reportCreateTool.name, mustResolveSchema(output))
	raw, err := json.Marshal(map[string]any{"request": reportArguments()})
	require.NoError(t, err)
	result, err := handler(ctx, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{
		Name: reportCreateTool.name, Arguments: raw,
	}})
	require.NoError(t, err)
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "report_outcome_unknown")
	require.EqualValues(t, 1, calls.Load())
}
