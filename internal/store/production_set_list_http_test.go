package store_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionSetListHTTPBoundsAndPages(t *testing.T) {
	vault, root, first, _, _ := store.ProductionReviewHTTPFixture(t)
	second, _, err := vault.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
		OperationID: "89000000-0000-4000-8000-000000000080", Name: "Second synthetic set"})
	require.NoError(t, err)
	listed, next, err := vault.ListProductionSets(t.Context(), "", 1)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.NotEmpty(t, next)
	continued, last, err := vault.ListProductionSets(t.Context(), next, 1)
	require.NoError(t, err)
	require.Len(t, continued, 1)
	require.Empty(t, last)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	get := func(query string, authorized bool) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			httpServer.URL+"/api/v1/productions/sets"+query, nil)
		require.NoError(t, err)
		if authorized {
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, body
	}
	status, _ := get("?limit=1", false)
	require.Equal(t, http.StatusUnauthorized, status)
	type setPage struct {
		Items      []redaction.Set `json:"items"`
		NextCursor string          `json:"next_cursor"`
	}
	status, body := get("?limit=1", true)
	require.Equal(t, http.StatusOK, status, string(body))
	var page setPage
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Items, 1)
	require.NotEmpty(t, page.NextCursor)
	firstID := page.Items[0].ID
	status, body = get("?limit=1&cursor="+page.NextCursor, true)
	require.Equal(t, http.StatusOK, status, string(body))
	page = setPage{}
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Items, 1)
	require.Empty(t, page.NextCursor)
	require.NotEqual(t, firstID, page.Items[0].ID)
	require.ElementsMatch(t, []string{first.ID, second.ID}, []string{firstID, page.Items[0].ID})
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientPage, err := client.ProductionSets(t.Context(), "", 1)
	require.NoError(t, err)
	require.Len(t, clientPage.Items, 1)
	require.Equal(t, firstID, clientPage.Items[0].ID)
	require.NotEmpty(t, clientPage.NextCursor)
	status, _ = get("?cursor=bad", true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = get("?limit=201", true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
}
