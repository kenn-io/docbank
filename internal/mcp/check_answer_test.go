package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestCheckAnswerTransports(t *testing.T) {
	server, citation := newCitationMCPFixture(t)
	citation.Start -= len([]rune(`$1,205 a`))
	citation.End = citation.Start + len([]rune(`$1,205`))
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
				"name": "check_answer", "arguments": map[string]any{"answer": "Invoice was $1,205 [1]", "citations": []any{citation}},
			}))
			output := objectField(t, result, "structuredContent")
			require.Equal(t, "grounded", output["status"])
			require.Equal(t, "private", output["cacheScope"])
			figures, ok := output["figures"].([]any)
			require.True(t, ok)
			require.Len(t, figures, 1)
			figure, ok := figures[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "$1,205", figure["text"])
			require.Equal(t, "found", figure["status"])
			assertSchemaAccepts(t, catalogMap(toolCatalog(ServerOptions{}))["check_answer"].OutputSchema, output)
		})
	}
}

func TestCheckAnswerRejectsInvalidBeforeAcquisition(t *testing.T) {
	var acquisitions atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		acquisitions.Add(1)
		return nil, errors.New("unexpected")
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	invalidCitation := citationMCPArguments()
	invalidCitation["start"] = 3
	for _, args := range []map[string]any{
		{"answer": "", "citations": []any{citationMCPArguments()}},
		{"answer": strings.Repeat("x", 16001), "citations": []any{citationMCPArguments()}},
		{"answer": "7", "citations": []any{}},
		{"answer": strings.Repeat("[1] ", 513), "citations": []any{citationMCPArguments()}},
		{"answer": "7", "citations": []any{invalidCitation}},
		{"answer": "7", "citations": []any{citationMCPArguments()}, "extra": true},
	} {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": args}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
	require.Zero(t, acquisitions.Load())
}

func TestCheckAnswerCitationFailures(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"citation_unavailable", 404}, {"citation_timeout", 504},
	} {
		t.Run(tc.code, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/text-citations/resolve", r.URL.Path)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"status":%d,"code":%q,"detail":"private detail"}`, tc.status, tc.code)
			}))
			t.Cleanup(daemon.Close)
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": map[string]any{"answer": "7", "citations": []any{citationMCPArguments()}}}))
			require.NotContains(t, string(raw), "private detail")
			result := decodeResult(t, raw)
			output := objectField(t, result, "structuredContent")
			if tc.code == "citation_unavailable" {
				require.Equal(t, "incomplete", output["status"])
				require.Equal(t, []any{map[string]any{"index": float64(1), "status": "citation_unavailable"}}, output["citations"])
			} else {
				require.Equal(t, true, result["isError"])
				require.Equal(t, tc.code, output["code"])
			}
		})
	}
}
