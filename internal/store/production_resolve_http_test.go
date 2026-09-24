package store_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionResolveHTTPReturnsExactBoundedMaskAndReviewBinding(t *testing.T) {
	vault, root, set, draft, member := store.ProductionReviewHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/sets/" + set.ID + "/revisions/1/resolve"
	call := func(body any, etag int64, authorized bool) (int, []byte) {
		t.Helper()
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if authorized {
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
		if etag > 0 {
			request.Header.Set("If-Match", `"`+strconv.FormatInt(etag, 10)+`"`)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	body := map[string]any{"member_id": member.ID, "page": 1, "limit": 1}
	status, _ := call(body, draft.ETag, false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = call(body, 0, true)
	require.Equal(t, http.StatusPreconditionRequired, status)
	status, _ = call(body, draft.ETag+1, true)
	require.Equal(t, http.StatusConflict, status)
	status, data := call(body, draft.ETag, true)
	require.Equal(t, http.StatusOK, status, string(data))
	require.NotContains(t, string(data), "synthetic relevance")
	var first api.ProductionResolvedMaskPage
	require.NoError(t, json.Unmarshal(data, &first))
	require.Equal(t, store.ProductionReviewBindingHTTPFixture(t, vault, set.ID, member.ID), first.ReviewBinding)
	require.Equal(t, member.MapSHA256, first.MapSHA256)
	require.Greater(t, first.TotalBoxes, 1)
	require.Len(t, first.Items, 1)
	require.NotEmpty(t, first.NextCursor)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	viaClient, err := client.ResolveProductionSelection(t.Context(), set.ID, 1, draft.ETag,
		api.ProductionResolveRequest{MemberID: member.ID, Page: 1, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, first, viaClient)
	complete, err := client.ResolveProductionSelection(t.Context(), set.ID, 1, draft.ETag,
		api.ProductionResolveRequest{MemberID: member.ID, Page: 1, Limit: 200})
	require.NoError(t, err)
	require.Len(t, complete.Items, first.TotalBoxes)
	require.Empty(t, complete.NextCursor)
	status, data = call(map[string]any{"member_id": member.ID, "page": 1, "limit": 1,
		"cursor": first.NextCursor}, draft.ETag, true)
	require.Equal(t, http.StatusOK, status, string(data))
	var second api.ProductionResolvedMaskPage
	require.NoError(t, json.Unmarshal(data, &second))
	require.NotEqual(t, first.Items[0], second.Items[0])
	status, _ = call(map[string]any{"member_id": member.ID, "page": 1, "limit": 1,
		"cursor": "not-a-cursor"}, draft.ETag, true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = call(map[string]any{"member_id": member.ID, "page": 2, "limit": 1}, draft.ETag, true)
	require.Equal(t, http.StatusNotFound, status)
}
