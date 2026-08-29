package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/processing"
)

const (
	testVaultID   = "11111111-1111-4111-8111-111111111111"
	testVersionID = "22222222-2222-4222-8222-222222222222"
)

var (
	testAttachmentID     = strings.Repeat("a", 64)
	testBuildID          = strings.Repeat("b", 64)
	testProfileID        = strings.Repeat("c", 64)
	testFenceFingerprint = func() string {
		fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{
			VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID},
		})
		if err != nil {
			panic(err)
		}
		return fingerprint
	}()
)

func TestNineReadToolHandlersReturnBoundedPrivateStructuredResults(t *testing.T) {
	daemon := newReadToolDaemon(t)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, "synthetic-key"), nil
	}, func(*client.Client) error { return nil })
	schemas := catalogMap(toolCatalog(false))

	tests := []struct {
		name      string
		arguments map[string]any
		check     func(*testing.T, map[string]any, *sdkmcp.CallToolResult)
	}{
		{name: "get_vault_info", arguments: map[string]any{}, check: func(t *testing.T, output map[string]any, _ *sdkmcp.CallToolResult) {
			t.Helper()
			assert.Equal(t, testVaultID, output["vault_id"])
			assert.NotContains(t, output, "vault_path")
		}},
		{name: "list_documents", arguments: map[string]any{"page_size": 1, "cursor": "opaque.token"}, check: func(t *testing.T, output map[string]any, result *sdkmcp.CallToolResult) {
			t.Helper()
			assert.Equal(t, "next.token", output["next_cursor"])
			assert.Contains(t, resourceLinkURIs(result), canonicalTestRenditionURI())
		}},
		{name: "search_documents", arguments: map[string]any{"query": "synthetic", "profile": "local",
			"mode": "lexical", "limit": 1, "content_version_ids": []string{testVersionID}},
			check: func(t *testing.T, output map[string]any, result *sdkmcp.CallToolResult) {
				t.Helper()
				assert.Equal(t, "lexical", output["actual_mode"])
				assert.Equal(t, testFenceFingerprint, output["fence_fingerprint"])
				assert.Equal(t, []any{"embedding_unavailable"}, output["skipped_reasons"])
				assert.Equal(t, []string{canonicalTestRenditionURI()}, resourceLinkURIs(result))
				coverage := objectField(t, output, "coverage")
				assert.Equal(t, false, coverage["binding_required"])
			}},
		{name: "get_document", arguments: map[string]any{"node_id": 7, "content_version_id": testVersionID},
			check: func(t *testing.T, _ map[string]any, result *sdkmcp.CallToolResult) {
				t.Helper()
				assert.Contains(t, resourceLinkURIs(result), canonicalTestRenditionURI())
			}},
		{name: "list_document_versions", arguments: map[string]any{"node_id": 7, "limit": 1, "offset": 0}},
		{name: "read_rendition_text", arguments: map[string]any{"vault_id": testVaultID, "node_id": 7,
			"content_version_id": testVersionID, "attachment_id": testAttachmentID, "offset": 1, "max_chars": 3},
			check: func(t *testing.T, output map[string]any, _ *sdkmcp.CallToolResult) {
				t.Helper()
				assert.Equal(t, "é界🙂", output["text"])
				assert.EqualValues(t, 4, output["next_offset"])
			}},
		{name: "get_processing_plan", arguments: map[string]any{"node_id": 7,
			"content_version_id": testVersionID, "profile": "local"}},
		{name: "get_processing_status", arguments: map[string]any{"job_id": strings.Repeat("e", 64)}},
		{name: "get_processing_coverage", arguments: map[string]any{"profile": "local", "vault_id": testVaultID,
			"content_version_ids": []string{testVersionID}}, check: func(t *testing.T, output map[string]any, _ *sdkmcp.CallToolResult) {
			t.Helper()
			coverage, ok := output["coverage"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, coverage)
			class, ok := coverage[0].(map[string]any)
			require.True(t, ok)
			assert.EqualValues(t, 0, class["previous_generation_serving"])
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := invokeReadTool(t.Context(), lease, test.name, test.arguments)
			require.NoError(t, err)
			require.False(t, result.IsError)
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			assert.LessOrEqual(t, len(encoded), maxToolResponseBytes)
			assert.NotContains(t, string(encoded), "/home/private")
			assert.NotContains(t, string(encoded), "raw-provider-response")
			output := structuredMap(t, result.StructuredContent)
			assertSchemaAccepts(t, schemas[test.name].OutputSchema, output)
			assert.EqualValues(t, 0, output["ttlMs"])
			assert.Equal(t, "private", output["cacheScope"])
			if test.check != nil {
				test.check(t, output, result)
			}
		})
	}
}

func TestReadToolCancellationPropagatesToDaemon(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(canceled)
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, ""), nil
	}, func(*client.Client) error { return nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := invokeReadTool(ctx, lease, "get_vault_info", map[string]any{})
		done <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	<-canceled
}

func TestReadToolResultCapFailsClosed(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/processing/plans" {
			writeDaemonJSON(t, response, api.ProcessingPlan{
				Fingerprint: strings.Repeat("a", 64), VaultUID: testVaultID,
				Selector:           api.ProcessingSelector{NodeID: 7, ContentVersionID: testVersionID, Profile: "local"},
				ProfileFingerprint: strings.Repeat("b", 64), Flow: []api.ProcessingFlowHop{},
				DisclosedClasses: []string{}, RetainedClasses: []string{},
				BackupConsequence: strings.Repeat("x", maxToolResponseBytes),
			})
			return
		}
		http.NotFound(response, request)
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, ""), nil
	}, func(*client.Client) error { return nil })
	_, err := invokeReadTool(t.Context(), lease, "get_processing_plan", map[string]any{
		"node_id": 7, "content_version_id": testVersionID, "profile": "local",
	})
	require.Error(t, err)
	assert.Equal(t, int64(-32603), sanitizedRPCError(err).Code)
}

func TestServerCapsCompleteToolResultIncludingServerMetadata(t *testing.T) {
	daemon := newReadToolDaemon(t)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, "synthetic-key"), nil
	}, func(*client.Client) error { return nil })
	implementation := testImplementation()
	implementation.Description = strings.Repeat("x", maxToolResponseBytes)
	server := newServerWithOptionsAndDaemon(implementation, ServerOptions{}, lease)

	wireErr := decodeWireError(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "get_vault_info", "arguments": map[string]any{},
	})))
	assert.Equal(t, int64(-32603), wireErr.Code)
	assert.LessOrEqual(t, len(wireErr.Message), maxToolErrorBytes)
}

func TestReadToolMapsSanitizedDaemonDomainErrors(t *testing.T) {
	for _, test := range []struct {
		name, tool, code string
		arguments        map[string]any
		status           int
	}{
		{name: "not found", tool: "get_vault_info", code: "not_found", status: http.StatusNotFound,
			arguments: map[string]any{}},
		{name: "invalid window", tool: "read_rendition_text", code: "invalid_rendition_window",
			status: http.StatusRequestedRangeNotSatisfiable, arguments: map[string]any{
				"vault_id": testVaultID, "node_id": 7, "content_version_id": testVersionID,
				"attachment_id": testAttachmentID, "offset": 999, "max_chars": 1,
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/problem+json")
				response.WriteHeader(test.status)
				writeDaemonJSON(t, response, map[string]any{
					"status": test.status, "title": "Request failed",
					"detail": "private /home/path raw-provider-response", "code": test.code,
				})
			}))
			t.Cleanup(daemon.Close)
			lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
				return client.New(daemon.URL, ""), nil
			}, func(*client.Client) error { return nil })
			result, err := invokeReadTool(t.Context(), lease, test.tool, test.arguments)
			require.NoError(t, err)
			require.True(t, result.IsError)
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			assert.Contains(t, string(encoded), `"code":"`+test.code+`"`)
			assert.NotContains(t, string(encoded), "/home/path")
			assert.NotContains(t, string(encoded), "raw-provider-response")
		})
	}
}

func TestReadToolMapsCursorCapacityToSanitizedWireResult(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/problem+json")
		response.WriteHeader(http.StatusServiceUnavailable)
		writeDaemonJSON(t, response, map[string]any{
			"status": http.StatusServiceUnavailable, "title": "Request failed",
			"detail": "private /home/path raw-provider-response", "code": "cursor_capacity",
		})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, ""), nil
	}, func(*client.Client) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)

	wire := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "list_documents", "arguments": map[string]any{},
	}))
	result := decodeResult(t, wire)
	assert.Equal(t, true, result["isError"])
	assert.Equal(t, "cursor_capacity", objectField(t, result, "structuredContent")["code"])
	assert.NotContains(t, string(wire), "/home/path")
	assert.NotContains(t, string(wire), "raw-provider-response")
}

func TestReadToolRejectsDaemonOutputOutsidePublishedSchema(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeDaemonJSON(t, response, api.VaultInfo{VaultID: "private-invalid-vault-id",
			VaultPath: "/home/private/vault"})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, ""), nil
	}, func(*client.Client) error { return nil })
	result, err := invokeReadTool(t.Context(), lease, "get_vault_info", map[string]any{})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.NotContains(t, err.Error(), "private-invalid")
	assert.NotContains(t, err.Error(), "/home/private")
}

func TestReadToolsRejectMismatchedDaemonAuthority(t *testing.T) {
	otherVersion := "33333333-3333-4333-8333-333333333333"
	otherJob := strings.Repeat("f", 64)
	for _, test := range []struct {
		name, tool string
		arguments  map[string]any
		path       string
		response   any
	}{
		{name: "plan selector", tool: "get_processing_plan", path: "/api/v1/processing/plans",
			arguments: map[string]any{"node_id": 7, "content_version_id": testVersionID, "profile": "local"},
			response: api.ProcessingPlan{Fingerprint: strings.Repeat("a", 64), VaultUID: testVaultID,
				Selector:           api.ProcessingSelector{NodeID: 7, ContentVersionID: otherVersion, Profile: "local"},
				ProfileFingerprint: testProfileID, Flow: []api.ProcessingFlowHop{},
				DisclosedClasses: []string{}, RetainedClasses: []string{}, ConsentState: "active",
				BackupConsequence: "synthetic"}},
		{name: "status job", tool: "get_processing_status", path: "/api/v1/processing/jobs/" + strings.Repeat("e", 64),
			arguments: map[string]any{"job_id": strings.Repeat("e", 64)},
			response: api.ProcessingStatus{JobID: otherJob, State: "completed", Phase: "embedding",
				EmbeddingJobIDs: []string{}}},
		{name: "version page", tool: "list_document_versions", path: "/api/v1/nodes/7/versions",
			arguments: map[string]any{"node_id": 7, "limit": 1, "offset": 0},
			response:  api.ContentVersionPage{Items: []api.ContentVersion{}, Total: 0, Limit: 2, Offset: 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/v1/nodes/7" {
					writeDaemonJSON(t, response, api.Node{ID: 7, Name: "synthetic.md", Kind: "file",
						CurrentVersionID: testVersionID, Path: "/synthetic.md"})
					return
				}
				assert.Equal(t, test.path, request.URL.Path)
				writeDaemonJSON(t, response, test.response)
			}))
			t.Cleanup(daemon.Close)
			lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
				return client.New(daemon.URL, ""), nil
			}, func(*client.Client) error { return nil })
			result, err := invokeReadTool(t.Context(), lease, test.tool, test.arguments)
			require.Error(t, err)
			assert.Nil(t, result)
			assert.NotContains(t, err.Error(), otherVersion)
			assert.NotContains(t, err.Error(), otherJob)
		})
	}
}

func TestSearchReadRejectsResultsOutsideResolvedFence(t *testing.T) {
	otherVersion := "33333333-3333-4333-8333-333333333333"
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/processing/source-fences/resolve":
			writeDaemonJSON(t, response, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{
				VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}},
				FenceFingerprint: testFenceFingerprint, ObservedScopeCount: 1})
		case "/api/v1/search":
			writeDaemonJSON(t, response, api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical",
				Coverage:     api.DocumentSearchCoverage{ScopedDocuments: 1, CompleteDocuments: 1, State: "complete"},
				Degradations: []string{}, Results: []api.DocumentSearchResult{{
					VaultUID: testVaultID, NodeID: 7, ContentVersionID: otherVersion,
					Rank: 1, Score: 1, Path: "/synthetic.md", Evidence: []api.DocumentEvidenceReference{},
				}}})
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, ""), nil
	}, func(*client.Client) error { return nil })

	result, err := invokeReadTool(t.Context(), lease, "search_documents", map[string]any{
		"query": "synthetic", "mode": "lexical", "limit": 1, "profile": "local",
		"content_version_ids": []string{testVersionID},
	})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.NotContains(t, err.Error(), otherVersion)
}

func TestSearchReadValidatesEmptyFenceWithoutCallingSearch(t *testing.T) {
	for _, test := range []struct {
		name, query, profile, wantCode string
		wantError                      bool
	}{
		{name: "valid empty result", query: "synthetic", profile: "local"},
		{name: "whitespace query", query: "   ", profile: "local", wantCode: "processing_failed", wantError: true},
		{name: "unconfigured profile", query: "synthetic", profile: "missing",
			wantCode: "processing_profile_unavailable", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			searchCalls := 0
			validationCalls := 0
			emptyFingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{
				VaultUID: testVaultID, ContentVersionIDs: []string{},
			})
			require.NoError(t, err)
			daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v1/processing/source-fences/resolve":
					writeDaemonJSON(t, response, api.DocumentSourceFenceResolution{
						Fence:            api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{}},
						FenceFingerprint: emptyFingerprint, ObservedScopeCount: 0,
					})
				case "/api/v1/search/validate":
					validationCalls++
					if test.wantError {
						status := http.StatusInternalServerError
						if test.wantCode == "processing_profile_unavailable" {
							status = http.StatusUnprocessableEntity
						}
						response.Header().Set("Content-Type", "application/problem+json")
						response.WriteHeader(status)
						writeDaemonJSON(t, response, map[string]any{
							"status": status, "title": "Request failed", "code": test.wantCode,
						})
						return
					}
					writeDaemonJSON(t, response, api.DocumentSearchValidation{Valid: true})
				case "/api/v1/search":
					searchCalls++
					http.Error(response, "search must not run for an empty fence", http.StatusInternalServerError)
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(daemon.Close)
			lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
				return client.New(daemon.URL, ""), nil
			}, func(*client.Client) error { return nil })

			result, err := invokeReadTool(t.Context(), lease, "search_documents", map[string]any{
				"query": test.query, "mode": "lexical", "limit": 1, "profile": test.profile,
				"filters": map[string]any{},
			})
			assert.Equal(t, 1, validationCalls)
			assert.Zero(t, searchCalls)
			if test.wantError {
				require.Error(t, err)
				facts, ok := daemonProblemFacts(err)
				require.True(t, ok)
				assert.Equal(t, test.wantCode, facts.Code)
				assert.Nil(t, result)
				return
			}
			require.NoError(t, err)
			output := structuredMap(t, result.StructuredContent)
			assert.Equal(t, []any{}, output["results"])
			assert.EqualValues(t, 0, output["observed_scope_count"])
		})
	}
}

func TestSearchReadWithNonEmptyFenceAndNoHitsSkipsDocumentResolution(t *testing.T) {
	resolveCalls := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/processing/source-fences/resolve":
			writeDaemonJSON(t, response, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{
				VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}},
				FenceFingerprint: testFenceFingerprint, ObservedScopeCount: 1})
		case "/api/v1/search":
			writeDaemonJSON(t, response, api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical",
				Coverage:     api.DocumentSearchCoverage{ScopedDocuments: 1, CompleteDocuments: 1, State: "complete"},
				Degradations: []string{}, Results: []api.DocumentSearchResult{}})
		case "/api/v1/documents/resolve":
			resolveCalls++
			t.Fatal("zero-hit search must not resolve document summaries")
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, ""), nil
	}, func(*client.Client) error { return nil })

	result, err := invokeReadTool(t.Context(), lease, "search_documents", map[string]any{
		"query": "synthetic", "mode": "lexical", "limit": 1, "profile": "local",
		"content_version_ids": []string{testVersionID},
	})
	require.NoError(t, err)
	assert.Zero(t, resolveCalls)
	assert.Empty(t, resourceLinkURIs(result))
	assert.Equal(t, []any{}, structuredMap(t, result.StructuredContent)["results"])
}

func TestSearchReadResolvesOneBoundedBatchOfCurrentDocumentsForResourceLinks(t *testing.T) {
	const resultCount = 100
	versions := make([]string, resultCount)
	results := make([]api.DocumentSearchResult, resultCount)
	identities := make([]struct {
		NodeID           int64  `json:"node_id"`
		ContentVersionID string `json:"content_version_id"`
		Path             string `json:"path"`
	}, resultCount)
	summaries := make([]api.DocumentSummary, resultCount)
	for index := range results {
		version := fmt.Sprintf("22222222-2222-4222-8222-%012d", index+1)
		path := fmt.Sprintf("/synthetic-%03d.md", index+1)
		versions[index] = version
		results[index] = api.DocumentSearchResult{VaultUID: testVaultID, NodeID: int64(index + 1),
			ContentVersionID: version, Rank: index + 1, Score: 1, Path: path,
			LexicalRank: index + 1, Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}
		identities[index] = struct {
			NodeID           int64  `json:"node_id"`
			ContentVersionID string `json:"content_version_id"`
			Path             string `json:"path"`
		}{NodeID: int64(index + 1), ContentVersionID: version, Path: path}
		summaries[index] = api.DocumentSummary{NodeID: int64(index + 1), ContentVersionID: version,
			Path: path, Name: path[1:], MediaType: "text/markdown", Size: 12,
			ModifiedAt: "2026-08-28T00:00:00Z", ActiveRenditions: []api.DocumentRenditionIdentity{{
				ProfileFingerprint: testProfileID, AttachmentID: testAttachmentID, BuildID: testBuildID,
			}}}
	}
	fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{
		VaultUID: testVaultID, ContentVersionIDs: versions,
	})
	require.NoError(t, err)
	batchCalls := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/processing/source-fences/resolve":
			writeDaemonJSON(t, response, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{
				VaultUID: testVaultID, ContentVersionIDs: versions}, FenceFingerprint: fingerprint,
				ObservedScopeCount: resultCount})
		case "/api/v1/search":
			writeDaemonJSON(t, response, api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical",
				Coverage: api.DocumentSearchCoverage{ScopedDocuments: resultCount, CompleteDocuments: resultCount,
					State: "complete"}, Degradations: []string{}, Results: results})
		case "/api/v1/documents/resolve":
			batchCalls++
			if !assert.Equal(t, http.MethodPost, request.Method) {
				return
			}
			var input struct {
				Identities []struct {
					NodeID           int64  `json:"node_id"`
					ContentVersionID string `json:"content_version_id"`
					Path             string `json:"path"`
				} `json:"identities"`
			}
			if !assert.NoError(t, json.UnmarshalRead(request.Body, &input)) {
				return
			}
			if !assert.Equal(t, identities, input.Identities) {
				return
			}
			writeDaemonJSON(t, response, struct {
				Items []api.DocumentSummary `json:"items"`
			}{Items: summaries})
		case "/api/v1/documents":
			t.Fatalf("search resource-link enrichment must not enumerate documents per hit")
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return client.New(daemon.URL, ""), nil
	}, func(*client.Client) error { return nil })

	result, err := invokeReadTool(t.Context(), lease, "search_documents", map[string]any{
		"query": "synthetic", "mode": "lexical", "limit": resultCount, "profile": "local",
		"content_version_ids": versions,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, batchCalls)
	assert.Len(t, resourceLinkURIs(result), resultCount)
}

func newReadToolDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "synthetic-key", request.Header.Get("X-Api-Key"))
		switch request.URL.Path {
		case "/api/v1/info":
			writeDaemonJSON(t, response, api.VaultInfo{VaultID: testVaultID, VaultPath: "/home/private/vault",
				LiveFiles: 1, LiveDirectories: 1, ContentVersions: 1, TrackedBlobs: 1})
		case "/api/v1/documents":
			if request.URL.Query().Has("cursor") {
				assert.Equal(t, "opaque.token", request.URL.Query().Get("cursor"))
			}
			pathPrefix := request.URL.Query().Get("path_prefix")
			if pathPrefix == "" {
				pathPrefix = "/"
			}
			writeDaemonJSON(t, response, api.DocumentPage{PathPrefix: pathPrefix, Sort: "path", Direction: "asc", PageSize: 1,
				Items: []api.DocumentSummary{{NodeID: 7, ContentVersionID: testVersionID,
					Path: "/synthetic.md", Name: "synthetic.md", MediaType: "text/markdown", Size: 12,
					ModifiedAt: "2026-08-28T00:00:00Z", ActiveRenditions: []api.DocumentRenditionIdentity{{
						ProfileFingerprint: testProfileID, AttachmentID: testAttachmentID, BuildID: testBuildID,
					}}}}, NextCursor: "next.token"})
		case "/api/v1/documents/resolve":
			var input api.DocumentSummaryResolveRequest
			if !assert.NoError(t, json.UnmarshalRead(request.Body, &input)) {
				return
			}
			if !assert.Len(t, input.Identities, 1) {
				return
			}
			if !assert.Equal(t, api.DocumentIdentity{NodeID: 7, ContentVersionID: testVersionID,
				Path: "/synthetic.md"}, input.Identities[0]) {
				return
			}
			writeDaemonJSON(t, response, api.DocumentSummaryResolveResponse{Items: []api.DocumentSummary{{
				NodeID: 7, ContentVersionID: testVersionID, Path: "/synthetic.md", Name: "synthetic.md",
				MediaType: "text/markdown", Size: 12, ModifiedAt: "2026-08-28T00:00:00Z",
				ActiveRenditions: []api.DocumentRenditionIdentity{{ProfileFingerprint: testProfileID,
					AttachmentID: testAttachmentID, BuildID: testBuildID}},
			}}})
		case "/api/v1/nodes/7":
			writeDaemonJSON(t, response, api.Node{ID: 7, Name: "synthetic.md", Kind: "file",
				CurrentVersionID: testVersionID, Size: 12, MimeType: "text/markdown",
				ModifiedAt: "2026-08-28T00:00:00Z", Path: "/synthetic.md"})
		case "/api/v1/nodes/7/versions":
			writeDaemonJSON(t, response, api.ContentVersionPage{Items: []api.ContentVersion{{ID: testVersionID,
				NodeID: 7, Size: 12, MimeType: "text/markdown", RecordedAt: "2026-08-28T00:00:00Z"}},
				Total: 1, Limit: 1, Offset: 0})
		case "/api/v1/processing/source-fences/resolve":
			var input api.DocumentSourceFenceResolveRequest
			if !assert.NoError(t, json.UnmarshalRead(request.Body, &input)) {
				return
			}
			assert.Equal(t, []string{testVersionID}, input.ContentVersionIDs)
			writeDaemonJSON(t, response, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{
				VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}},
				FenceFingerprint: testFenceFingerprint, ObservedScopeCount: 1})
		case "/api/v1/search":
			var input api.DocumentSearchRequest
			if !assert.NoError(t, json.UnmarshalRead(request.Body, &input)) {
				return
			}
			assert.Equal(t, api.DocumentSourceFence{VaultUID: testVaultID,
				ContentVersionIDs: []string{testVersionID}}, input.Fence)
			writeDaemonJSON(t, response, api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical",
				Coverage:     api.DocumentSearchCoverage{ScopedDocuments: 1, CompleteDocuments: 1, State: "complete"},
				Degradations: []string{"embedding_unavailable"}, Results: []api.DocumentSearchResult{{
					VaultUID: testVaultID, NodeID: 7, ContentVersionID: testVersionID,
					Rank: 1, Score: 1, Path: "/synthetic.md", Excerpt: "synthetic", LexicalRank: 1,
					Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}},
				}}})
		case "/api/v1/renditions/windows":
			writeDaemonJSON(t, response, api.RenditionTextWindow{VaultID: testVaultID, NodeID: 7,
				ContentVersionID: testVersionID, AttachmentID: testAttachmentID,
				BuildID: testBuildID, ProfileFingerprint: testProfileID, Text: "é界🙂",
				MediaType: "text/markdown", Checksum: strings.Repeat("d", 64),
				RequestedOffset: 1, ActualStart: 1, ActualEnd: 4, NextOffset: 4,
				ResponseBytes: len("é界🙂")})
		case "/api/v1/processing/plans":
			writeDaemonJSON(t, response, api.ProcessingPlan{Fingerprint: strings.Repeat("d", 64),
				VaultUID: testVaultID, Selector: api.ProcessingSelector{NodeID: 7,
					ContentVersionID: testVersionID, Profile: "local"}, ProfileFingerprint: testProfileID,
				Flow: []api.ProcessingFlowHop{}, DisclosedClasses: []string{}, RetainedClasses: []string{},
				ConsentState: "active", BackupConsequence: "derivatives are backed up"})
		case "/api/v1/processing/jobs/" + strings.Repeat("e", 64):
			writeDaemonJSON(t, response, api.ProcessingStatus{JobID: strings.Repeat("e", 64),
				State: "completed", Phase: "embedding", EmbeddingJobIDs: []string{}, CompletedBindings: 0})
		case "/api/v1/coverage":
			writeDaemonJSON(t, response, api.CoverageReport{VaultUID: testVaultID,
				ProfileFingerprint: testProfileID, State: "complete",
				Renditions: api.CoverageClass{Name: "rendition", Required: true, State: "complete", Complete: 1, Total: 1},
				Embeddings: []api.CoverageClass{}})
		default:
			http.NotFound(response, request)
		}
	}))
}

func writeDaemonJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.MarshalWrite(response, value))
}

func structuredMap(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(encoded, &result))
	return result
}

func resourceLinkURIs(result *sdkmcp.CallToolResult) []string {
	var uris []string
	for _, content := range result.Content {
		if link, ok := content.(*sdkmcp.ResourceLink); ok {
			uris = append(uris, link.URI)
		}
	}
	return uris
}

func canonicalTestRenditionURI() string {
	return "docbank://vaults/" + testVaultID + "/documents/7/versions/" +
		testVersionID + "/renditions/" + testAttachmentID
}
