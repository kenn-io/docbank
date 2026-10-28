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
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestCheckAnswerTransports(t *testing.T) {
	server, citation := newCitationMCPFixture(t)
	citation.Start -= len([]rune(`$1,205 a`))
	citation.End = citation.Start + len([]rune(`$1,205`))
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			resolved := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{"name": "resolve_text_citation", "arguments": citation}))
			require.Equal(t, `$1,205`, objectField(t, resolved, "structuredContent")["text"])
			for _, tc := range []struct{ answer, status, figureStatus string }{
				{"Invoice was $1,250 [1]", "ungrounded", "missing"},
				{"Invoice was $1,205 [1]", "grounded", "found"},
			} {
				result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
					"name": "check_answer", "arguments": map[string]any{"answer": tc.answer, "citations": []any{citation}},
				}))
				output := objectField(t, result, "structuredContent")
				require.Equal(t, tc.status, output["status"])
				require.Equal(t, "private", output["cacheScope"])
				figures, ok := output["figures"].([]any)
				require.True(t, ok)
				require.Len(t, figures, 1)
				figure, ok := figures[0].(map[string]any)
				require.True(t, ok)
				require.Equal(t, tc.answer[12:18], figure["text"])
				require.Equal(t, tc.figureStatus, figure["status"])
			}
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
	for _, args := range []map[string]any{
		{"answer": "", "citations": []any{citationMCPArguments()}},
		{"answer": strings.Repeat("x", 16001), "citations": []any{citationMCPArguments()}},
		{"answer": "7", "citations": []any{}},
		{"answer": strings.Repeat("[1] ", 513), "citations": []any{citationMCPArguments()}},
		{"answer": strings.Repeat("K5 ", 513), "citations": []any{citationMCPArguments()}},
		{"answer": "7", "citations": []any{map[string]any{}}},
		{"answer": "7", "citations": []any{citationMCPArguments()}, "extra": true},
	} {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": args}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
	require.Zero(t, acquisitions.Load())
}

func TestCheckAnswerCitationFailuresAndDuplicates(t *testing.T) {
	for _, tc := range []struct {
		code    string
		status  int
		allowed bool
	}{
		{"citation_unavailable", 404, true}, {"citation_integrity", 500, true},
		{"citation_timeout", 504, false}, {"citation_failed", 500, false},
		{"invalid_citation_range", 416, true}, {"citation_limit", 413, true},
		{"citation_canceled", 408, false}, {"unknown_problem", 500, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var calls atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"status":%d,"code":%q,"detail":"private detail"}`, tc.status, tc.code)
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
			args := citationMCPArguments()
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": map[string]any{"answer": "7", "citations": []any{args, args, args, args, args, args, args, args}}}))
			require.NotContains(t, string(raw), "private detail")
			if tc.allowed {
				output := objectField(t, decodeResult(t, raw), "structuredContent")
				require.Equal(t, "incomplete", output["status"])
				citations, ok := output["citations"].([]any)
				require.True(t, ok)
				require.Len(t, citations, 8)
				for _, entry := range citations {
					citation, ok := entry.(map[string]any)
					require.True(t, ok)
					require.Equal(t, tc.code, citation["status"])
				}
				require.EqualValues(t, 1, calls.Load())
			} else if tc.code == "citation_canceled" || tc.code == "citation_timeout" || tc.code == "citation_failed" {
				result := decodeResult(t, raw)
				require.Equal(t, true, result["isError"])
				require.Equal(t, tc.code, objectField(t, result, "structuredContent")["code"])
			} else {
				require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
			}
		})
	}
}
