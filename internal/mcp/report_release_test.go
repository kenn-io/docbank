package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

func TestMCPReportRelease(t *testing.T) {
	f := newNativeExportFixture(t)
	summary, err := f.connection.CreateTermReport(t.Context(), report.Request{
		Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}}},
	})
	require.NoError(t, err)
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowReportWrites: true}, f.lease)
	args := map[string]any{"report_id": summary.ID}
	output := exportCall(t, server, "release_report", args)
	require.Equal(t, map[string]any{"ttlMs": float64(0), "cacheScope": "private",
		"report_id": summary.ID, "released": true}, output)
	_, schema := releaseReportSchemas()
	assertSchemaAccepts(t, schema, output)
	output["unexpected"] = true
	assertSchemaRejects(t, schema, output)
	delete(output, "unexpected")
	output["released"] = false
	assertSchemaRejects(t, schema, output)
	_, err = f.connection.GetTermReport(t.Context(), summary.ID)
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "report_unavailable", code)
	require.Equal(t, "report_unavailable", exportCall(t, server, "release_report", args)["code"])
}

func TestMCPReportReleaseInput(t *testing.T) {
	var calls atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		calls.Add(1)
		return nil, errDaemonUnavailable
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowReportWrites: true}, lease)
	for _, args := range []any{nil, map[string]any{}, map[string]any{"report_id": nil},
		map[string]any{"report_id": "invalid"},
		map[string]any{"report_id": strings.Repeat("A", 48)},
		map[string]any{"report_id": strings.Repeat("a", 48), "extra": true},
	} {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": "release_report", "arguments": args,
		}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
	require.Zero(t, calls.Load())
}

func TestMCPReportReleaseFailures(t *testing.T) {
	id := strings.Repeat("a", 48)
	for _, tc := range []struct {
		name, code string
		status     int
	}{
		{"retained", "report_retained", 409},
		{"gone", "report_unavailable", 410},
		{"unavailable service", "report_unavailable", 503},
		{"lost reply", "report_outcome_unknown", 0},
		{"canceled", "report_outcome_unknown", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodDelete, r.Method)
				assert.Equal(t, "/api/v1/search-exports/"+id, r.URL.Path)
				if tc.name == "canceled" {
					cancel()
					<-r.Context().Done()
					return
				}
				if tc.status == 0 {
					panic(http.ErrAbortHandler)
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"status":%d,"title":"Conflict","code":%q}`,
					tc.status, tc.code)
			}))
			defer daemon.Close()
			tools := &reportTools{lease: exportTestLease(t, daemon.URL),
				logger: slog.New(slog.DiscardHandler)}
			_, output := releaseReportSchemas()
			handler := tools.handler(reportReleaseTool.name, mustResolveSchema(output))
			result, err := handler(ctx, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{
				Name: "release_report", Arguments: []byte(`{"report_id":"` + id + `"}`),
			}})
			require.NoError(t, err)
			encoded, err := json.Marshal(result.StructuredContent)
			require.NoError(t, err)
			var reply map[string]any
			require.NoError(t, json.Unmarshal(encoded, &reply))
			require.Equal(t, tc.code, reply["code"])
			require.NotContains(t, reply, "released")
			if tc.code == "report_outcome_unknown" {
				require.Contains(t, reply["message"], "write may have succeeded")
				require.NotContains(t, reply["message"], "created a report")
			}
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestMCPReportReleaseResultFailure(t *testing.T) {
	f := newNativeExportFixture(t)
	summary, err := f.connection.CreateTermReport(t.Context(), report.Request{
		Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}}},
	})
	require.NoError(t, err)
	tools := &reportTools{lease: f.lease, logger: slog.New(slog.DiscardHandler)}
	handler := tools.handler(reportReleaseTool.name, mustResolveSchema(rootObjectSchema(schema{})))
	result, err := handler(t.Context(), &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{
		Name: "release_report", Arguments: []byte(`{"report_id":"` + summary.ID + `"}`),
	}})
	require.NoError(t, err)
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "report_outcome_unknown")
	_, err = f.connection.GetTermReport(t.Context(), summary.ID)
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "report_unavailable", code, "mutation succeeded despite rejected output")
}
