package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

type similarCountingProvider struct {
	processingTestEmbeddingProvider

	calls atomic.Int64
}

func (p *similarCountingProvider) Embed(ctx context.Context, inputs []document.EmbeddingInput, auth document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	p.calls.Add(1)
	return p.processingTestEmbeddingProvider.Embed(ctx, inputs, auth)
}

func TestSimilarDocumentsRouteGroupsCopiesAndNeverEmbeds(t *testing.T) {
	provider := &similarCountingProvider{processingTestEmbeddingProvider: newProcessingTestEmbeddingProvider(t)}
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbeddingProvider(t, provider, true))
	c := daemonconn.New(ts.URL, testAPIKey)
	var nodes []store.Node
	for _, item := range []struct{ name, text string }{{"source", "shared synthetic text"}, {"copy", "shared synthetic text"}, {"copy-two", "shared synthetic text"}, {"distinct", "different synthetic text"}} {
		node := createFileWithContent(t, ts, catalog, "/"+item.name+".txt", item.text)
		nodes = append(nodes, node)
		selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
		plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
		require.NoError(t, err)
		_, err = c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
		require.NoError(t, err)
	}
	request := api.DocumentSimilarRequest{Selector: api.ProcessingSelector{NodeID: nodes[0].ID, ContentVersionID: nodes[0].CurrentVersionID, Profile: "private"}, BindingID: "semantic", Limit: 20, Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID()}}
	for _, node := range nodes {
		request.Fence.ContentVersionIDs = append(request.Fence.ContentVersionIDs, node.CurrentVersionID)
	}
	provider.calls.Store(0)
	report, err := c.SimilarDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "ready", report.State)
	require.Len(t, report.Results, 2)
	assert.Equal(t, nodes[1].ID, report.Results[0].NodeID)
	assert.Equal(t, 1, report.Results[0].DuplicateCount)
	assert.NotEqual(t, report.Results[0].BlobHash, report.Results[1].BlobHash)
	for _, hit := range report.Results {
		assert.NotEqual(t, nodes[0].ID, hit.NodeID)
	}
	assert.Zero(t, provider.calls.Load(), "provider_calls=0")
	t.Log("source node excluded; identical copy remains; provider_calls=0")
	request.Limit = 1
	limited, err := c.SimilarDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, limited.Results, 1)
	assert.True(t, limited.Truncated)
	assert.Equal(t, 1, limited.Results[0].DuplicateCount, "duplicate_count=1 before limit")
	request.Fence.ContentVersionIDs = []string{nodes[0].CurrentVersionID, nodes[2].CurrentVersionID}
	fenced, err := c.SimilarDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, fenced.Results, 1)
	assert.Equal(t, nodes[2].ID, fenced.Results[0].NodeID)
	assert.Zero(t, fenced.Results[0].DuplicateCount)
	request.Fence.ContentVersionIDs = []string{nodes[0].CurrentVersionID}
	empty, err := c.SimilarDocuments(t.Context(), request)
	require.NoError(t, err)
	assert.Empty(t, empty.Results)
	assert.Equal(t, "ready", empty.State)

	db, err := catalog.SQLiteDriver().Open(catalog.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `UPDATE vector_index_generations SET source_manifest_checksum=?`, strings.Repeat("f", 64))
	require.NoError(t, err)
	for _, test := range []struct {
		path   string
		body   any
		status int
		code   string
	}{
		{"/api/v1/search/similar", request, http.StatusConflict, "stale_index"},
		{"/api/v1/search", api.DocumentSearchRequest{Query: "synthetic", Mode: "semantic", Profile: "private", BindingID: "semantic", Limit: 20, Fence: request.Fence}, http.StatusInternalServerError, "processing_failed"},
	} {
		t.Run(test.path+"/stale_index", func(t *testing.T) {
			response, body := do(t, ts, http.MethodPost, test.path, nil, test.body)
			assert.Equal(t, test.status, response.StatusCode, body)
			assert.Contains(t, body, `"code":"`+test.code+`"`)
		})
	}
}

func TestSimilarMissingCoverageRequiresSourceFenceAndCurrentIdentity(t *testing.T) {
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbedding(t))
	node := createFileWithContent(t, ts, catalog, "/unembedded.txt", "synthetic missing head")
	c := daemonconn.New(ts.URL, testAPIKey)
	request := api.DocumentSimilarRequest{Selector: api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}, Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}}}
	report, err := c.SimilarDocuments(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, "unavailable", report.State)
	require.NotNil(t, report.MissingCoverage)
	assert.Equal(t, "embedding", report.MissingCoverage.Kind)
	for len(request.Fence.ContentVersionIDs) < 4096 {
		request.Fence.ContentVersionIDs = append(request.Fence.ContentVersionIDs, uuid.New().String())
	}
	_, err = c.SimilarDocuments(t.Context(), request)
	require.NoError(t, err, "4096 fence boundary")
	request.Limit = 101
	response, body := do(t, ts, http.MethodPost, "/api/v1/search/similar", nil, request)
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	t.Log("4096 source versions accepted; limit=101 rejected")
	request.Limit = 20
	request.Fence.ContentVersionIDs = request.Fence.ContentVersionIDs[1:]
	response, body = do(t, ts, http.MethodPost, "/api/v1/search/similar", nil, request)
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	assert.Contains(t, body, `"code":"validation"`)
	request.Fence.ContentVersionIDs = []string{node.CurrentVersionID}
	request.Selector.NodeID++
	response, body = do(t, ts, http.MethodPost, "/api/v1/search/similar", nil, request)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	request.Selector.NodeID = node.ID
	hash, size, err := catalog.Blobs.Write(strings.NewReader("changed synthetic source"))
	require.NoError(t, err)
	updated, _, err := catalog.ReplaceContent(t.Context(), node.ID, store.UnconditionalRev, hash, size, node.MimeType)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/search/similar", nil, request)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body, "historical source stays an error")
	request.Selector.ContentVersionID = updated.CurrentVersionID
	request.Fence.ContentVersionIDs = []string{updated.CurrentVersionID}
	_, _, err = catalog.Trash(t.Context(), node.ID, store.UnconditionalRev)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/search/similar", nil, request)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body, "trashed source stays an error")
}
