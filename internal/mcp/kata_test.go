package mcp

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		{"selector": map[string]any{"node_id": 7, "start_rune": 0, "end_rune": 3, "unexpected": true}, "title": "Unknown", "idempotency_key": "key"},
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
	for _, code := range []string{"kata_incompatible", "kata_unconfigured", "invalid_kata_evidence", "kata_idempotency_conflict", "kata_evidence_changed", "kata_evidence_unavailable", "kata_evidence_unsupported", "kata_timeout", "kata_request_failed", "kata_unauthorized", "kata_forbidden", "kata_issue_unavailable"} {
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
	assertSchemaAccepts(t, tools["get_kata_context"].InputSchema, map[string]any{"issue": "docbank#abcd", "offset": kataevidence.MaxContextOffset})
	assertSchemaRejects(t, tools["get_kata_context"].InputSchema, map[string]any{"issue": "docbank#abcd", "offset": kataevidence.MaxContextOffset + 1})
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
			Issue:    kataclient.Issue{UID: "01TEST", ShortID: "abcd", QualifiedRef: "docbank#abcd", Title: "Synthetic issue", Body: strings.Repeat("large issue body", 10000)},
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

func TestKataContextReturnsDaemonErrors(t *testing.T) {
	for _, acquireFailure := range []bool{true, false} {
		t.Run(strconv.FormatBool(acquireFailure), func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection, _, err := http.NewResponseController(w).Hijack()
				if !assert.NoError(t, err) {
					return
				}
				_ = connection.Close()
			}))
			defer daemon.Close()
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				if acquireFailure {
					return nil, errDaemonUnavailable
				}
				return daemonconn.New(daemon.URL, "synthetic-key"), nil
			}, func(c *daemonconn.Connection) error { return c.Close() })
			_, err := getKataContext(t.Context(), lease, []byte(`{"issue":"docbank#abcd"}`))
			require.Error(t, err)
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
