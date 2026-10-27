package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestCheckAnswerTransports(t *testing.T) {
	server, citation := newCitationMCPFixture(t)
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			listed := decodeResult(t, exportExchange(t, server, transport, "tools/list", nil))
			tool := listedToolsByName(t, listed)["check_answer"]
			require.NotNil(t, tool)
			hints := objectField(t, tool, "annotations")
			require.Equal(t, true, hints["readOnlyHint"])
			require.Equal(t, true, hints["idempotentHint"])
			result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
				"name": "check_answer", "arguments": map[string]any{"answer": "There are 7 items [1].", "citations": []any{citation}},
			}))
			output := objectField(t, result, "structuredContent")
			require.Equal(t, "ungrounded", output["status"])
			require.Equal(t, "private", output["cacheScope"])
			encodedBytes, err := json.Marshal(output)
			require.NoError(t, err)
			encoded := string(encodedBytes)
			require.NotContains(t, encoded, "é界🙂")
		})
	}
	t.Run("saved citation range", func(t *testing.T) {
		citation := citation
		citation.Start += 5
		citation.End = citation.Start + len([]rune("2024. Revenue increased"))
		resolved := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "resolve_text_citation", "arguments": citation})))
		require.Equal(t, "2024. Revenue increased", objectField(t, resolved, "structuredContent")["text"])
		result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": map[string]any{"answer": "Revenue rose in 2024 [1].", "citations": []any{citation}}})))
		require.Equal(t, "grounded", objectField(t, result, "structuredContent")["status"])
		result = decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": map[string]any{"answer": "Revenue rose in &#50;024 [1].", "citations": []any{citation}}})))
		output := objectField(t, result, "structuredContent")
		require.Equal(t, "grounded", output["status"])
		claims, ok := output["claims"].([]any)
		require.True(t, ok)
		require.Len(t, claims, 1)
		claim, ok := claims[0].(map[string]any)
		require.True(t, ok)
		figures, ok := claim["figures"].([]any)
		require.True(t, ok)
		require.Len(t, figures, 1)
		figure, ok := figures[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "found", figure["status"])
		require.Contains(t, figure["values"], "2024")
		citation.Start += 6
		result = decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": map[string]any{"answer": "Revenue rose in 2024 [1].", "citations": []any{citation}}})))
		require.Equal(t, "ungrounded", objectField(t, result, "structuredContent")["status"])
	})
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
		{"citation_timeout", 504, true}, {"citation_failed", 500, true},
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
			} else if tc.code == "citation_canceled" {
				require.Equal(t, true, decodeResult(t, raw)["isError"])
			} else {
				require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
			}
		})
	}
}

func TestCheckAnswerResolvedAndBounded(t *testing.T) {
	args := citationMCPArguments()
	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"citation": args, "text": "123", "text_bytes": 3,
			"text_sha256": "a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3"})
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	for _, tc := range []struct {
		answer, status  string
		claims, figures int
	}{
		{"123 [1]", "grounded", 1, 1}, {"123 [0]", "ungrounded", 1, 1},
		{strings.Repeat("123 ", 512), "grounded", 1, 512},
		{strings.Repeat("123 123. ", 256), "grounded", 256, 512},
		{strings.Repeat("123 123 [1,2]. ", 256), "grounded", 256, 512},
		{strings.Repeat("123456789012345678901234567890 ", 512), "ungrounded", 1, 512},
	} {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": map[string]any{"answer": tc.answer, "citations": []any{args, args, args, args, args, args, args, args}}}))
		require.Less(t, len(raw), maxToolResponseBytes)
		output := objectField(t, decodeResult(t, raw), "structuredContent")
		require.Equal(t, tc.status, output["status"])
		claims, ok := output["claims"].([]any)
		require.True(t, ok)
		require.Len(t, claims, tc.claims)
		figureCount := 0
		for _, item := range claims {
			claim, ok := item.(map[string]any)
			require.True(t, ok)
			figures, ok := claim["figures"].([]any)
			require.True(t, ok)
			figureCount += len(figures)
		}
		require.Equal(t, tc.figures, figureCount)
	}
	require.EqualValues(t, 6, calls.Load())
	for _, tc := range []struct{ name, answer string }{
		{"maximum leading decimal", "." + strings.Repeat("1", 15999)},
		{"maximum negative leading decimal", "-." + strings.Repeat("1", 15998)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": map[string]any{"answer": tc.answer, "citations": []any{args}}}))
			require.Less(t, len(raw), maxToolResponseBytes)
			require.Equal(t, "ungrounded", objectField(t, decodeResult(t, raw), "structuredContent")["status"])
		})
	}
}

func TestCheckAnswerMalformedResponseFailsWholeTool(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"private text"}`))
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": map[string]any{"answer": "123", "citations": []any{citationMCPArguments()}}}))
	require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
	require.NotContains(t, string(raw), "private text")
}

func TestCheckAnswerFailureAfterResolvedCitationDoesNotReplay(t *testing.T) {
	for _, mode := range []string{"disconnect", "unauthorized", "forbidden", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			first := citationMCPArguments()
			second := maps.Clone(first)
			second["node_id"] = 8
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					_ = json.MarshalWrite(w, map[string]any{"citation": first, "text": "123", "text_bytes": 3, "text_sha256": "a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3"})
					return
				}
				switch mode {
				case "disconnect":
					conn, _, err := http.NewResponseController(w).Hijack()
					if err != nil {
						t.Errorf("hijack response: %v", err)
						return
					}
					_ = conn.Close()
				case "unauthorized":
					w.WriteHeader(http.StatusUnauthorized)
				case "forbidden":
					w.WriteHeader(http.StatusForbidden)
				case "unavailable":
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"status":404,"code":"citation_unavailable","detail":"secret"}`))
				}
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "check_answer", "arguments": map[string]any{"answer": "123 [1]. 456 [2].", "citations": []any{first, second}}}))
			require.NotContains(t, string(raw), "secret")
			require.EqualValues(t, 2, calls.Load())
			if mode == "unavailable" {
				output := objectField(t, decodeResult(t, raw), "structuredContent")
				require.Equal(t, "incomplete", output["status"])
				citations, ok := output["citations"].([]any)
				require.True(t, ok)
				require.Len(t, citations, 2)
				for i, status := range []string{"resolved", "citation_unavailable"} {
					citation, ok := citations[i].(map[string]any)
					require.True(t, ok)
					require.Equal(t, status, citation["status"])
				}
			} else {
				var response map[string]any
				require.NoError(t, json.Unmarshal(raw, &response))
				if result, ok := response["result"].(map[string]any); ok {
					require.Equal(t, true, result["isError"])
				} else {
					require.NotNil(t, response["error"])
				}
			}
		})
	}
}

func TestCheckAnswerCancellationBetweenResolves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// PutIdleConn runs when the first response body has been consumed.
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{PutIdleConn: func(err error) {
		if err == nil {
			cancel()
		}
	}})
	first := citationMCPArguments()
	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"citation": first, "text": "123", "text_bytes": 3,
			"text_sha256": "a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3"})
	}))
	defer daemon.Close()
	raw, err := json.Marshal(map[string]any{"answer": "123", "citations": []any{first, first}})
	require.NoError(t, err)
	_, err = checkAnswer(ctx, exportTestLease(t, daemon.URL), raw)
	require.ErrorIs(t, err, context.Canceled)
	require.EqualValues(t, 1, calls.Load())
}
