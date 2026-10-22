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
	"unicode/utf8"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
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
	var acquisitions atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		acquisitions.Add(1)
		return nil, errDaemonUnavailable
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	for _, transport := range []string{"stdio", "http"} {
		raw := exportExchange(t, server, transport, "tools/call", map[string]any{"name": "cite_kata_evidence", "arguments": kataCiteArguments()})
		require.NotZero(t, decodeWireError(t, raw).Code)
	}
	require.Zero(t, acquisitions.Load())
	tools := catalogMap(toolCatalog(ServerOptions{AllowKataWrites: true}))
	require.False(t, tools["cite_kata_evidence"].Annotations.ReadOnlyHint)
	require.True(t, tools["cite_kata_evidence"].Annotations.IdempotentHint)
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
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"cite_kata_evidence", map[string]any{"issue": "docbank#abcd"}},
		{"get_kata_context", map[string]any{"issue": ""}},
	} {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": tc.tool, "arguments": tc.args}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
	require.Zero(t, acquisitions.Load())
}

func TestKataDomainErrorsDoNotExposeDaemonDetails(t *testing.T) {
	for code, status := range map[string]int{
		"kata_unconfigured": 503, "kata_unauthorized": 401, "kata_forbidden": 403,
		"kata_issue_unavailable": 404, "kata_incompatible": 503, "invalid_kata_evidence": 422,
		"kata_idempotency_conflict": 409, "kata_evidence_changed": 409, "kata_evidence_unavailable": 404,
		"kata_evidence_limit": 409, "kata_evidence_unsupported": 409, "kata_timeout": 504,
		"kata_request_failed": 502,
	} {
		t.Run(code, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(status)
				_ = json.MarshalWrite(w, map[string]any{"status": status, "code": code, "detail": "private secret source text"})
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

func TestKataContextSchemaBoundsOffset(t *testing.T) {
	tools := catalogMap(toolCatalog(ServerOptions{}))
	assertSchemaAccepts(t, tools["get_kata_context"].InputSchema, map[string]any{"issue": "docbank#abcd", "offset": kataevidence.MaxContextOffset})
	assertSchemaRejects(t, tools["get_kata_context"].InputSchema, map[string]any{"issue": "docbank#abcd", "offset": kataevidence.MaxContextOffset + 1})
}

func TestKataContextReadsFakeDaemonAndProjectsBoundedReceipt(t *testing.T) {
	future := kataReferenceFixture()
	future.Version = 2
	rendition := kataevidence.Reference{Version: 1, Kind: "docbank", Rendition: &document.TextCitation{
		Version: 1, VaultUID: uuid.MustParse("00000000-0000-4000-8000-000000000001"), NodeID: 7,
		ContentVersionID: uuid.MustParse("00000000-0000-4000-8000-000000000007"), ContentSHA256: strings.Repeat("a", 64),
		RenditionAttachmentID: strings.Repeat("b", 64), BuildID: strings.Repeat("c", 64), RenditionSHA256: strings.Repeat("d", 64), Start: 0, End: 3,
	}}
	for _, tc := range []struct {
		name       string
		passages   []kataevidence.Resolution
		nextOffset int
	}{
		{"rendition", []kataevidence.Resolution{{State: "available", SavedQuote: "abc", Evidence: kataevidence.NewEvidence(rendition, "abc", "Synthetic text")}}, 11},
		{"unsupported_version", []kataevidence.Resolution{{State: "unsupported", SavedQuote: "abc", Evidence: kataevidence.NewEvidence(future, "", "Synthetic text")}}, 11},
		{"unsupported_reference", []kataevidence.Resolution{
			{State: "unsupported", Evidence: kataevidence.Evidence{Reference: kataevidence.Reference{Kind: "docbank"}, ContentTrust: "untrusted"}, SavedQuote: "saved words"},
			{State: "available", Evidence: kataevidence.NewEvidence(kataReferenceFixture(), "abc", "Synthetic text"), SavedQuote: "abc"},
		}, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
					Passages: tc.passages, NextOffset: tc.nextOffset,
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, page))
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
			output := exportCall(t, server, "get_kata_context", map[string]any{"issue": "docbank#abcd", "offset": 10})
			require.Equal(t, "private", output["cacheScope"])
			require.EqualValues(t, 0, output["ttlMs"])
			require.Equal(t, map[string]any{"uid": "01TEST", "qualified_ref": "docbank#abcd", "title": "Synthetic issue"}, objectField(t, output, "issue"))
			require.EqualValues(t, tc.nextOffset, output["next_offset"])
			passages, ok := output["passages"].([]any)
			require.True(t, ok)
			require.Len(t, passages, len(tc.passages))
			for i, expected := range tc.passages {
				passage, ok := passages[i].(map[string]any)
				require.True(t, ok)
				require.Equal(t, expected.State, passage["state"])
				require.Equal(t, expected.SavedQuote, passage["saved_quote"])
				if tc.name == "unsupported_reference" && i == 0 {
					require.NotContains(t, passage, "evidence", "omit an unsupported reference instead of asserting its shape")
				} else {
					evidence := objectField(t, passage, "evidence")
					require.Equal(t, "untrusted", evidence["content_trust"])
					if tc.name == "rendition" {
						require.Equal(t, strings.Repeat("c", 64), objectField(t, objectField(t, evidence, "reference"), "rendition")["build_id"])
					}
					if tc.name == "unsupported_version" {
						require.EqualValues(t, 2, objectField(t, evidence, "reference")["version"])
					}
				}
			}
			require.NotContains(t, output, "body")
			require.Equal(t, 1, calls)
		})
	}
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
			want := errDaemonRequestFailed
			if acquireFailure {
				want = errDaemonUnavailable
			}
			require.ErrorIs(t, err, want)
			require.Equal(t, want.Error(), err.Error())
		})
	}
}

func TestKataReceiptBoundsExistingIssueTitle(t *testing.T) {
	issue := kataclient.Issue{UID: "01SYNTHETIC", QualifiedRef: "docbank#abcd", Title: strings.Repeat("🙂", 600)}
	got := kataReceipt(issue)
	require.LessOrEqual(t, utf8.RuneCountInString(got.Title), 512)
	require.True(t, strings.HasSuffix(got.Title, "…"))
	require.Equal(t, issue.UID, got.UID)
	require.Equal(t, issue.QualifiedRef, got.QualifiedRef)
}
