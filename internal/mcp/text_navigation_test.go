package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMCPTextNavigation(t *testing.T) {
	server, citation := newCitationMCPFixture(t)
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			listed := listedToolsByName(t, decodeResult(t,
				exportExchange(t, server, transport, "tools/list", nil)))
			for _, name := range []string{"get_text_outline", "read_text_section"} {
				require.NotNil(t, listed[name])
				hints := objectField(t, listed[name], "annotations")
				require.Equal(t, true, hints["readOnlyHint"])
				require.Equal(t, true, hints["idempotentHint"])
				require.Equal(t, false, hints["destructiveHint"])
			}
			call := func(name string, args any) map[string]any {
				result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
					"name": name, "arguments": args}))
				output := objectField(t, result, "structuredContent")
				require.Equal(t, "private", output["cacheScope"])
				require.EqualValues(t, 0, output["ttlMs"])
				return output
			}
			anchor := citation
			anchor.Start = 0
			anchor.End = 1
			output := call("get_text_outline", document.TextOutlineRequest{Citation: anchor})
			encoded, err := json.Marshal(output)
			require.NoError(t, err)
			var outline document.TextOutlinePage
			require.NoError(t, json.Unmarshal(encoded, &outline))
			require.Equal(t, anchor, outline.Citation)
			require.NotEmpty(t, outline.Sections)
			request := document.TextSectionRequest{Citation: anchor,
				Section: outline.EnclosingSection, MaxChars: 3}
			output = call("read_text_section", request)
			encoded, err = json.Marshal(output)
			require.NoError(t, err)
			var first document.TextSectionPage
			require.NoError(t, json.Unmarshal(encoded, &first))
			require.False(t, first.Complete)
			require.Equal(t, 3, first.NextOffset)
			derived := anchor
			derived.Start = first.ActualStart
			derived.End = first.ActualEnd
			quote := call("resolve_text_citation", derived)
			require.Equal(t, first.Text, quote["text"])
			require.Equal(t, first.TextSHA256, quote["text_sha256"])
			request.Offset = first.NextOffset
			request.MaxChars = 16000
			output = call("read_text_section", request)
			require.Equal(t, true, output["complete"])
			tail, ok := output["text"].(string)
			require.True(t, ok)
			require.Contains(t, first.Text+tail, "aé界🙂z")
			request.Offset = request.Section.End - request.Section.Start
			output = call("read_text_section", request)
			require.Empty(t, output["text"])
			require.EqualValues(t, 0, output["text_bytes"])
			require.Equal(t, true, output["complete"])
		})
	}
}

func TestMCPTextNavigationInvalidArguments(t *testing.T) {
	var acquisitions atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		acquisitions.Add(1)
		return nil, errors.New("unexpected acquisition")
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	for _, name := range []string{"get_text_outline", "read_text_section"} {
		valid := map[string]any{"citation": citationMCPArguments(), "offset": 0}
		if name == "read_text_section" {
			valid["section"] = map[string]any{"start": 0, "end": 30000}
			valid["max_chars"] = 16000
		}
		var invalid []map[string]any
		for field := range valid {
			missing := maps.Clone(valid)
			delete(missing, field)
			invalid = append(invalid, missing)
			null := maps.Clone(valid)
			null[field] = nil
			invalid = append(invalid, null)
		}
		for _, change := range []map[string]any{{"extra": true}, {"offset": -1}, {"offset": 2147483648}} {
			bad := maps.Clone(valid)
			maps.Copy(bad, change)
			invalid = append(invalid, bad)
		}
		bad := maps.Clone(valid)
		c := citationMCPArguments()
		c["end"] = 0
		bad["citation"] = c
		invalid = append(invalid, bad)
		if name == "read_text_section" {
			bad = maps.Clone(valid)
			bad["section"] = map[string]any{"start": 2, "end": 1}
			invalid = append(invalid, bad)
		}
		for _, args := range invalid {
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": name, "arguments": args}))
			require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code, args)
		}
	}
	require.Zero(t, acquisitions.Load())
}

func TestMCPTextNavigationBounds(t *testing.T) {
	for _, name := range []string{"get_text_outline", "read_text_section"} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"citation": citationMCPArguments(), "offset": 0}
			text := strings.Repeat("\x01", 16000)
			var response any
			if name == "read_text_section" {
				section := map[string]any{"start": 0, "end": 16000}
				args["section"] = section
				args["max_chars"] = 16000
				response = map[string]any{"citation": args["citation"], "section": section, "offset": 0,
					"actual_start": 0, "actual_end": 16000, "next_offset": 16000, "complete": true,
					"text": text, "text_bytes": len(text),
					"text_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(text)))}
			} else {
				args["offset"] = 1
				entries := make([]document.TextOutlineEntry, 50)
				for i := range entries {
					entries[i] = document.TextOutlineEntry{Start: i * 200, End: (i + 1) * 200,
						Level: 1, TitleMarkdown: strings.Repeat("\x01", 160)}
				}
				response = map[string]any{"citation": args["citation"], "document_chars": 10000,
					"enclosing_section": map[string]any{"start": 0, "end": 200}, "offset": 1,
					"total": 51, "sections": entries}
			}
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, response)
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{},
				exportTestLease(t, daemon.URL))
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": name, "arguments": args}))
			require.Less(t, len(raw), maxToolResponseBytes)
			output := objectField(t, decodeResult(t, raw), "structuredContent")
			if name == "read_text_section" {
				require.Equal(t, text, output["text"])
			} else {
				require.Len(t, output["sections"], 50)
			}
		})
	}
}

func TestMCPTextNavigationDomainFailures(t *testing.T) {
	for _, code := range []string{"invalid_text_navigation", "text_navigation_limit"} {
		t.Run(code, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = fmt.Fprintf(w, `{"status":422,"code":%q,"detail":"private detail"}`, code)
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{},
				exportTestLease(t, daemon.URL))
			output := exportCall(t, server, "get_text_outline", map[string]any{
				"citation": citationMCPArguments(), "offset": 0})
			require.Equal(t, code, output["code"])
			require.NotContains(t, fmt.Sprint(output), "private detail")
		})
	}
}
