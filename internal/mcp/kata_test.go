package mcp

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/docbank/internal/kataevidence"
)

func kataReferenceFixture() kataevidence.Reference {
	return kataevidence.Reference{Version: 1, Kind: "docbank", Docbank: &kataevidence.DocumentReference{
		VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: 7,
		ContentVersionID: "00000000-0000-4000-8000-000000000007", ContentSHA256: strings.Repeat("a", 64), StartRune: 0, EndRune: 3,
	}}
}

func kataCiteArguments() map[string]any {
	return map[string]any{"selector": map[string]any{"node_id": 7, "start_rune": 0, "end_rune": 3, "quote": "abc"}, "title": "Synthetic issue", "brief": "Review synthetic passage", "idempotency_key": "synthetic-action"}
}

func TestKataWritesRequireTheirOwnOptIn(t *testing.T) {
	for _, options := range []ServerOptions{
		{}, {AllowProcessing: true}, {AllowPackageWrites: true}, {AllowPhotoEdits: true}, {AllowExportWrites: true}, {AllowReportWrites: true},
		{AllowProcessing: true, AllowPackageWrites: true, AllowPhotoEdits: true, AllowExportWrites: true, AllowReportWrites: true},
		{AllowKataWrites: true},
	} {
		var acquisitions atomic.Int32
		lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
			acquisitions.Add(1)
			return nil, errDaemonUnavailable
		}, func(*daemonconn.Connection) error { return nil })
		server := newServerWithOptionsAndDaemon(testImplementation(), options, lease)
		tools := catalogMap(toolCatalog(options))
		require.Contains(t, tools, "get_kata_context")
		if options.AllowKataWrites {
			require.Contains(t, tools, "cite_kata_evidence")
			require.False(t, tools["cite_kata_evidence"].Annotations.ReadOnlyHint)
			require.True(t, tools["cite_kata_evidence"].Annotations.IdempotentHint)
			require.NotContains(t, tools, "start_processing")
			require.NotContains(t, tools, "create_report")
		} else {
			require.NotContains(t, tools, "cite_kata_evidence")
			for _, transport := range []string{"stdio", "http"} {
				raw := exportExchange(t, server, transport, "tools/call", map[string]any{"name": "cite_kata_evidence", "arguments": kataCiteArguments()})
				require.NotZero(t, decodeWireError(t, raw).Code)
			}
		}
		require.Zero(t, acquisitions.Load())
	}
}

func TestKataCiteUsesFakeDaemonWithoutRetry(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "receipt", true: "lost reply"}[lost], func(t *testing.T) {
			var calls, acquisitions atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/kata/cite", r.URL.Path)
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					return
				}
				want, err := json.Marshal(kataCiteArguments())
				if !assert.NoError(t, err) {
					return
				}
				assert.JSONEq(t, string(want), string(body))
				if lost {
					connection, _, err := http.NewResponseController(w).Hijack()
					if !assert.NoError(t, err) {
						return
					}
					_ = connection.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, kataclient.Issue{UID: "01TEST", ShortID: "abcd", QualifiedRef: "docbank#abcd", Title: "Synthetic issue", Body: "Unrelated source text"}))
			}))
			defer daemon.Close()
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				acquisitions.Add(1)
				c := daemonconn.New(daemon.URL, "synthetic-key")
				t.Cleanup(func() { require.NoError(t, c.Close()) })
				return c, nil
			}, func(c *daemonconn.Connection) error { return c.Close() })
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowKataWrites: true}, lease)
			result := exportCall(t, server, "cite_kata_evidence", kataCiteArguments())
			if lost {
				require.Equal(t, "kata_outcome_unknown", result["code"])
				require.Contains(t, result["message"], "same")
			} else {
				require.Equal(t, "private", result["cacheScope"])
				require.EqualValues(t, 0, result["ttlMs"])
				require.Equal(t, map[string]any{"uid": "01TEST", "qualified_ref": "docbank#abcd", "title": "Synthetic issue"}, objectField(t, result, "issue"))
			}
			require.Equal(t, int32(1), calls.Load())
			require.Equal(t, int32(1), acquisitions.Load())
		})
	}
}

func TestKataInputValidationPrecedesDaemonAcquisition(t *testing.T) {
	var acquisitions atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		acquisitions.Add(1)
		return nil, errDaemonUnavailable
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowKataWrites: true}, lease)
	for _, args := range []map[string]any{
		{"issue": "docbank#abcd"},
		{"selector": map[string]any{"node_id": 7, "start_rune": 0, "end_rune": 1001}, "title": "Too much", "idempotency_key": "key"},
		{"selector": map[string]any{"node_id": 7, "start_rune": 5, "end_rune": 3}, "title": "Backward", "idempotency_key": "key"},
		{"selector": map[string]any{"node_id": 7, "start_rune": 0, "end_rune": 3}, "title": "No key"},
		{"selector": map[string]any{"node_id": 7, "start_rune": 0, "end_rune": 3, "unexpected": true}, "title": "Unknown", "idempotency_key": "key"},
		{"selector": map[string]any{"node_id": 7, "start_rune": 0, "end_rune": 3}, "issue": "docbank#abcd", "title": "Append title"},
	} {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "cite_kata_evidence", "arguments": args}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
	for _, args := range []map[string]any{{"issue": ""}, {"issue": "docbank#abcd", "offset": 321}, {"issue": "docbank#abcd", "secret": true}} {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "get_kata_context", "arguments": args}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
	require.Zero(t, acquisitions.Load())
}

func TestKataDomainErrorsDoNotExposeDaemonDetails(t *testing.T) {
	for _, code := range []string{"kata_incompatible", "kata_unconfigured", "invalid_kata_evidence", "kata_idempotency_conflict", "kata_evidence_changed", "kata_evidence_unavailable", "kata_evidence_unsupported", "kata_timeout", "kata_request_failed"} {
		t.Run(code, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.MarshalWrite(w, map[string]any{"status": 503, "code": code, "detail": "private secret source text"})
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
			output := exportCall(t, server, "get_kata_context", map[string]any{"issue": "docbank#abcd"})
			require.Equal(t, code, output["code"])
			require.NotContains(t, output["message"], "private")
			require.NotContains(t, output["message"], "secret")
		})
	}
}

func TestKataReadToolIsInDefaultCatalog(t *testing.T) {
	tools := catalogMap(toolCatalog(ServerOptions{}))
	require.Contains(t, tools, "get_kata_context")
	require.NotContains(t, tools, "cite_kata_evidence")
	assertSchemaAccepts(t, tools["get_kata_context"].InputSchema, map[string]any{"issue": "docbank#abcd", "offset": 320})
}

func TestKataContextReadsFakeDaemonAndProjectsBoundedReceipt(t *testing.T) {
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/kata/context", r.URL.Path)
		assert.Equal(t, "synthetic-export-key", r.Header.Get("X-Api-Key"))
		var request kataevidence.ContextRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &request))
		assert.Equal(t, kataevidence.ContextRequest{Issue: "docbank#abcd", Offset: 10}, request)
		page := kataevidence.Context{
			Issue:    kataclient.Issue{UID: "01TEST", ShortID: "abcd", QualifiedRef: "docbank#abcd", Title: "Synthetic issue", Body: strings.Repeat("large issue body", 10000), Comments: []kataclient.Comment{{Body: "private unrelated comment"}}},
			Passages: []kataevidence.Resolution{{State: "changed", SavedQuote: "abc", Evidence: kataevidence.NewEvidence(kataReferenceFixture(), "abc", "Synthetic text")}}, NextOffset: 11,
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, page))
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	output := exportCall(t, server, "get_kata_context", map[string]any{"issue": "docbank#abcd", "offset": 10})
	require.Equal(t, "private", output["cacheScope"])
	require.EqualValues(t, 0, output["ttlMs"])
	receipt := objectField(t, output, "issue")
	require.Equal(t, map[string]any{"uid": "01TEST", "qualified_ref": "docbank#abcd", "title": "Synthetic issue"}, receipt)
	require.EqualValues(t, 11, output["next_offset"])
	passages, ok := output["passages"].([]any)
	require.True(t, ok)
	require.Len(t, passages, 1)
	passage, ok := passages[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "changed", passage["state"])
	require.Equal(t, "abc", passage["saved_quote"])
	require.Equal(t, "untrusted", objectField(t, passage, "evidence")["content_trust"])
	require.NotContains(t, output, "body")
	require.NotContains(t, receipt, "comments")
	require.Equal(t, 1, calls)
}

func TestKataContextKeepsUnsupportedVersionAndSavedQuote(t *testing.T) {
	reference := kataReferenceFixture()
	reference.Version = 2
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := kataevidence.Context{Issue: kataclient.Issue{UID: "01TEST", ShortID: "abcd", QualifiedRef: "docbank#abcd", Title: "Synthetic issue"}, Passages: []kataevidence.Resolution{{State: "unsupported", SavedQuote: "abc", Evidence: kataevidence.NewEvidence(reference, "", "Synthetic text")}}}
		assert.NoError(t, json.MarshalWrite(w, page))
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	output := exportCall(t, server, "get_kata_context", map[string]any{"issue": "docbank#abcd"})
	passages, ok := output["passages"].([]any)
	require.True(t, ok)
	require.Len(t, passages, 1)
	passage, ok := passages[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "unsupported", passage["state"])
	require.Equal(t, "abc", passage["saved_quote"])
	evidence := objectField(t, passage, "evidence")
	require.EqualValues(t, 2, objectField(t, evidence, "reference")["version"])
}

func TestKataContextOfflineFallbackRequiresRequestTransportFailure(t *testing.T) {
	for _, authenticated := range []bool{true, false} {
		t.Run(map[bool]string{true: "unreachable daemon", false: "authentication refusal"}[authenticated], func(t *testing.T) {
			var kataCalls atomic.Int32
			evidence := kataevidence.NewEvidence(kataReferenceFixture(), "abc", "Synthetic text")
			entry := kataevidence.Entry{ID: evidence.ID, Passage: evidence.Passage, Reference: evidence.Reference}
			metadata, err := json.Marshal(map[string]any{"version": 1, "entries": []kataevidence.Entry{entry}})
			require.NoError(t, err)
			kata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				kataCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, _ = io.WriteString(w, `{"api_schema_version":"0.21.0"}`)
					return
				}
				if r.URL.Path == "/api/v1/projects" {
					_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"docbank","active":true}]}`)
					return
				}
				assert.Equal(t, "/api/v1/projects/7/issues/abcd", r.URL.Path)
				_, _ = io.WriteString(w, `{"issue":{"uid":"01TEST","short_id":"abcd","title":"Synthetic issue","body":"Evidence: Synthetic text\n\n> abc\n","metadata":{"docbank.evidence":`+string(metadata)+`}},"comments":[]}`)
			}))
			defer kata.Close()
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			t.Setenv("DOCBANK_MCP_SYNTHETIC_KATA_KEY", "synthetic-key")
			cfg := "[integrations.kata]\nenabled = true\nendpoint = \"" + kata.URL + "\"\napi_key_env = \"DOCBANK_MCP_SYNTHETIC_KATA_KEY\"\ndefault_project = \"docbank\"\n"
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600))
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !authenticated {
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"status":401,"code":"unauthorized","detail":"private refusal"}`)
					return
				}
				connection, _, err := http.NewResponseController(w).Hijack()
				if !assert.NoError(t, err) {
					return
				}
				_ = connection.Close()
			}))
			defer daemon.Close()
			recovered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := kataevidence.Context{Issue: kataclient.Issue{UID: "01TEST", ShortID: "abcd", QualifiedRef: "docbank#abcd", Title: "Synthetic issue"}, Passages: []kataevidence.Resolution{{State: "available", Evidence: evidence, SavedQuote: "abc"}}}
				data, err := json.Marshal(page)
				if !assert.NoError(t, err) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(data)
			}))
			defer recovered.Close()
			var acquisitions atomic.Int32
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				base := daemon.URL
				if acquisitions.Add(1) > 1 {
					base = recovered.URL
				}
				connection := daemonconn.New(base, "synthetic-export-key")
				t.Cleanup(func() { require.NoError(t, connection.Close()) })
				return connection, nil
			}, func(c *daemonconn.Connection) error { return c.Close() })
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "get_kata_context", "arguments": map[string]any{"issue": "docbank#abcd"}}))
			if !authenticated {
				require.NotZero(t, decodeWireError(t, raw).Code)
				require.Zero(t, kataCalls.Load(), "authentication refusals must not read local Kata credentials")
				return
			}
			output := objectField(t, decodeResult(t, raw), "structuredContent")
			passages, ok := output["passages"].([]any)
			require.True(t, ok)
			require.Len(t, passages, 1)
			passage, ok := passages[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "unreachable", passage["state"])
			require.Equal(t, "abc", passage["saved_quote"])
			require.Equal(t, int32(3), kataCalls.Load())
			retry := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "get_kata_context", "arguments": map[string]any{"issue": "docbank#abcd"}}))
			readback := objectField(t, decodeResult(t, retry), "structuredContent")
			readbackPassages, ok := readback["passages"].([]any)
			require.True(t, ok)
			require.Len(t, readbackPassages, 1)
			recoveredPassage, ok := readbackPassages[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "available", recoveredPassage["state"], "retry must acquire the recovered daemon at its new endpoint")
			require.Equal(t, int32(2), acquisitions.Load())
			require.Equal(t, int32(3), kataCalls.Load(), "recovered reads use the daemon")
		})
	}
}

func TestKataUnsupportedReferenceKeepsOtherPassagesReadable(t *testing.T) {
	evidence := kataevidence.NewEvidence(kataReferenceFixture(), "abc", "Synthetic text")
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, kataevidence.Context{
			Issue: kataclient.Issue{UID: "01TEST", QualifiedRef: "docbank#abcd", Title: "Synthetic issue"},
			Passages: []kataevidence.Resolution{
				{State: "unsupported", Evidence: kataevidence.Evidence{Reference: kataevidence.Reference{Kind: "docbank"}, ContentTrust: "untrusted"}, SavedQuote: "saved words"},
				{State: "available", Evidence: evidence, SavedQuote: "abc"},
			},
		}))
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	output := exportCall(t, server, "get_kata_context", map[string]any{"issue": "docbank#abcd"})
	passages, ok := output["passages"].([]any)
	require.True(t, ok, "%v", output)
	require.Len(t, passages, 2)
	first, ok := passages[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "unsupported", first["state"])
	require.Equal(t, "saved words", first["saved_quote"])
	require.NotContains(t, first, "evidence", "omit an unsupported reference instead of asserting its shape")
	second, ok := passages[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "available", second["state"])
}
