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
	for _, tc := range []struct{ answer, status, figureStatus, figureText string }{
		{"Invoice was $1,205 [1]", "grounded", "found", "$1,205"},
		{"€1,205 [1]", "ungrounded", "missing", "€1,205"},
	} {
		result := decodeResult(t, exportExchange(t, server, "stdio", "tools/call", map[string]any{
			"name": "check_answer", "arguments": map[string]any{"answer": tc.answer, "citations": []any{citation}},
		}))
		output := objectField(t, result, "structuredContent")
		require.Equal(t, tc.status, output["status"])
		figures, ok := output["figures"].([]any)
		require.True(t, ok)
		require.Len(t, figures, 1)
		figure, ok := figures[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, tc.figureStatus, figure["status"])
		require.Equal(t, "private", output["cacheScope"])
		require.Equal(t, tc.figureText, figure["text"])
		assertSchemaAccepts(t, catalogMap(toolCatalog(ServerOptions{}))["check_answer"].OutputSchema, output)
	}
}

func TestCheckAnswerRetainedNormalization(t *testing.T) {
	wideDate := "March" + strings.Repeat(" ", 64) + "15, 2026"
	for _, tc := range []struct{ mediaType, body, quote, answer, status string }{
		{"text/html", "<p>$1&nbsp;205</p>", "$1 205", "USD 1", "ungrounded"},
		{"text/html", "<p>$1&nbsp;205</p>", "$1 205", "$1", "grounded"},
		{"text/html", "<p>$1&nbsp;205</p>", "$1 205", "$1 205", "grounded"},
		{"text/html", "<p>2026-03-15</p>", "2026-03-15", "March 15, 2026", "grounded"},
		{"text/html", "<p>1,250 USD</p>", "1,250 USD", "$1,250", "grounded"},
		{"text/plain", "Price: $40\n\n205 units", "Price: $40\n\n205 units", "$40", "grounded"},
		{"text/plain", "USD 40 2026-03-15", "USD 40 2026-03-15", "$40 on March 15, 2026", "grounded"},
		{"text/plain", wideDate, wideDate, "March 15, 2026", "grounded"},
		{"text/plain", "USD 40\nCAD 30", "USD 40\nCAD 30", "$40 C$30", "grounded"},
		{"text/plain", "2026-03-15", "2026-03-15", "March\u202815,\u20282026", "grounded"},
		{"text/plain", "2026-03-15", "2026-03-15", "15\u2009March\u20092026", "grounded"},
		{"text/plain", "$1,250.00", "$1,250.00", "$1,250", "grounded"},
	} {
		t.Run(tc.answer+"/"+tc.quote, func(t *testing.T) {
			server, citation := newCitationMCPFixtureText(t, tc.mediaType, tc.body, tc.quote)
			for _, transport := range []string{"stdio", "http"} {
				result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
					"name": "check_answer", "arguments": map[string]any{"answer": tc.answer, "citations": []any{citation}},
				}))
				require.Equal(t, tc.status, objectField(t, result, "structuredContent")["status"])
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
	invalidCitation := citationMCPArguments()
	invalidCitation["start"] = 3
	for _, args := range []map[string]any{
		{"answer": "", "citations": []any{citationMCPArguments()}},
		{"answer": strings.Repeat("x", 16001), "citations": []any{citationMCPArguments()}},
		{"answer": "7", "citations": []any{}},
		{"answer": "7", "citations": []any{citationMCPArguments(), citationMCPArguments(), citationMCPArguments(), citationMCPArguments(), citationMCPArguments(), citationMCPArguments(), citationMCPArguments(), citationMCPArguments(), citationMCPArguments()}},
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
