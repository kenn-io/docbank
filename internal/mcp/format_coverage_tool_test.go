package mcp

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	documentcoverage "go.kenn.io/docbank/document/coverage"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	internalformatcoverage "go.kenn.io/docbank/internal/formatcoverage"
	"go.kenn.io/docbank/internal/processing"
)

func TestFormatCoverageToolReturnsVerifiedFilteredInventory(t *testing.T) {
	tool := catalogMap(toolCatalog(false, false))["get_format_coverage"]
	require.NotNil(t, tool)
	require.True(t, tool.Annotations.ReadOnlyHint)
	require.True(t, tool.Annotations.IdempotentHint)

	snapshot, err := internalformatcoverage.Compute(nil, processing.SourceMetadataExtractorFingerprint)
	require.NoError(t, err)
	lookup := documentcoverage.Lookup(snapshot, "pdf")
	require.NotNil(t, lookup.Format)
	require.Nil(t, lookup.Format.Variants)
	standardWire, err := stdjson.Marshal(api.FormatCoverageResponse{FormatCoverageV1: snapshot})
	require.NoError(t, err)
	require.True(t, bytes.Contains(standardWire, []byte(`"variants":null`)))
	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodGet, request.Method)
		assert.Equal(t, "/api/v1/formats/capabilities", request.URL.Path)
		assert.Empty(t, request.URL.Query().Get("extension"))
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("format") == "" {
			assert.NoError(t, stdjson.NewEncoder(response).Encode(api.FormatCoverageResponse{FormatCoverageV1: snapshot}))
			return
		}
		assert.Equal(t, "pdf", request.URL.Query().Get("format"))
		filtered := snapshot
		filtered.Formats = filtered.Formats[:0:0]
		filtered.Formats = append(filtered.Formats, *lookup.Format)
		assert.NoError(t, stdjson.NewEncoder(response).Encode(api.FormatCoverageResponse{FormatCoverageV1: filtered, Lookup: &lookup}))
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	result, err := invokeReadTool(t.Context(), lease, "get_format_coverage", map[string]any{"format": "pdf"})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.EqualValues(t, 1, calls.Load())
	var output map[string]any
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &output))
	require.Equal(t, "format-coverage/v1", output["contract_version"])
	require.Equal(t, "private", output["cacheScope"])
	require.Len(t, output["formats"], 1)
	selectedFormats, ok := output["formats"].([]any)
	require.True(t, ok)
	selected, ok := selectedFormats[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []any{}, selected["variants"])
	direct, err := daemonconn.New(daemon.URL, "synthetic-key").FormatCapabilities(t.Context(), "", "", "")
	require.NoError(t, err)
	require.Greater(t, len(direct.Formats), 1)
	full, err := invokeReadTool(t.Context(), lease, "get_format_coverage", map[string]any{})
	require.NoError(t, err)
	require.False(t, full.IsError)
	require.EqualValues(t, 3, calls.Load())
	raw, err = json.Marshal(full.StructuredContent)
	require.NoError(t, err)
	output = map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &output))
	formats, ok := output["formats"].([]any)
	require.True(t, ok)
	require.Greater(t, len(formats), 1)
	for _, item := range formats {
		format, ok := item.(map[string]any)
		require.True(t, ok)
		_, ok = format["variants"].([]any)
		require.True(t, ok)
	}
	require.NotContains(t, output, "lookup")
}

func TestFormatCoverageToolRejectsDualSelectorBeforeDaemonCall(t *testing.T) {
	wireErr := decodeWireError(t, callToolWire(t, "get_format_coverage", map[string]any{
		"format": "pdf", "extension": ".pdf",
	}))
	require.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code)

	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		response.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	_, err := invokeReadTool(t.Context(), lease, "get_format_coverage", map[string]any{
		"format": "pdf", "extension": ".pdf",
	})
	require.Error(t, err)
	require.Zero(t, calls.Load())
}

func TestFormatCoverageToolRejectsInvalidDaemonInventory(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeDaemonJSON(t, response, api.FormatCoverageResponse{})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	result, err := invokeReadTool(t.Context(), lease, "get_format_coverage", map[string]any{})
	require.Error(t, err)
	require.Nil(t, result)
}
