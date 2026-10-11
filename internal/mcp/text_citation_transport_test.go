package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMCPTextCitationDomainFailures(t *testing.T) {
	for code, status := range map[string]int{
		"invalid_text_citation": 422, "citation_unavailable": 404, "citation_limit": 413,
		"invalid_citation_range": 416, "citation_timeout": 504, "citation_canceled": 408,
		"citation_integrity": 500, "citation_failed": 500,
	} {
		t.Run(code, func(t *testing.T) {
			var calls atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"status":%d,"code":%q,"detail":"private detail"}`, status, code)
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{},
				exportTestLease(t, daemon.URL))
			output := exportCall(t, server, "resolve_text_citation", citationMCPArguments())
			require.Equal(t, code, output["code"])
			require.NotContains(t, fmt.Sprint(output), "private detail")
			require.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestMCPTextCitationResponseBounds(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		name := "maximum escaped quote"
		if invalid {
			name = "invalid response"
		}
		t.Run(name, func(t *testing.T) {
			args := citationMCPArguments()
			args["end"] = 16000
			text := strings.Repeat("\x01", 16000)
			var calls atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				output := map[string]any{"citation": args, "text": text, "text_bytes": len(text),
					"text_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(text)))}
				if invalid {
					output["text_bytes"] = 0
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, output)
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{},
				exportTestLease(t, daemon.URL))
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "resolve_text_citation", "arguments": args,
			}))
			require.Less(t, len(raw), maxToolResponseBytes)
			if invalid {
				require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
			} else {
				result := decodeResult(t, raw)
				require.Equal(t, text, objectField(t, result, "structuredContent")["text"])
			}
			require.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestMCPTextCitationReadRetry(t *testing.T) {
	for _, started := range []bool{false, true} {
		name := "retry before response"
		if started {
			name = "no retry after response"
		}
		t.Run(name, func(t *testing.T) {
			args := citationMCPArguments()
			var requests, acquisitions atomic.Int32
			received := make(chan map[string]any, 2)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got map[string]any
				if err := json.UnmarshalRead(r.Body, &got); err != nil {
					t.Error(err)
					return
				}
				received <- got
				if requests.Add(1) == 1 {
					if started {
						w.Header().Set("Content-Length", "500")
						_, _ = io.WriteString(w, `{"citation":`)
						return
					}
					connection, _, err := http.NewResponseController(w).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, map[string]any{"citation": args, "text": "abc", "text_bytes": 3,
					"text_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte("abc")))})
			}))
			defer daemon.Close()
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				acquisitions.Add(1)
				connection := daemonconn.New(daemon.URL, "synthetic-key")
				t.Cleanup(func() { require.NoError(t, connection.Close()) })
				return connection, nil
			}, func(c *daemonconn.Connection) error { return c.Close() })
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "resolve_text_citation", "arguments": args,
			}))
			wantCalls := int32(2)
			if started {
				wantCalls = 1
				require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
			} else {
				require.Equal(t, "abc", objectField(t, decodeResult(t, raw), "structuredContent")["text"])
			}
			require.Equal(t, wantCalls, requests.Load())
			require.Equal(t, wantCalls, acquisitions.Load())
			canonical, err := json.Marshal(args)
			require.NoError(t, err)
			for range wantCalls {
				encoded, err := json.Marshal(<-received)
				require.NoError(t, err)
				require.JSONEq(t, string(canonical), string(encoded))
			}
		})
	}
}
