package store_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestProductionDecisionUncertaintyPagesHTTPAndClient(t *testing.T) {
	vault, root, set, draft, _ := store.ProductionReviewHTTPFixture(t)
	decisions, _, err := vault.ProductionDecisions(t.Context(), set.ID, draft.Revision, "", 1)
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	added := decisions[0]
	added.ID = "89000000-0000-4000-8000-000000000021"
	added.Actor, added.CreatedAt, added.Revision = "", "", 0
	added.Uncertain = true
	second := added
	second.ID = "89000000-0000-4000-8000-000000000023"
	_, err = vault.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, draft.Revision,
		redaction.ApplyRequest{OperationID: "89000000-0000-4000-8000-000000000022", ETag: draft.ETag,
			Changes: []redaction.Change{{Kind: "decision", Decision: &added}, {Kind: "decision", Decision: &second}}})
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/sets/" + set.ID + "/revisions/1/decisions"
	get := func(query string, authorized bool) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpServer.URL+path+query, nil)
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
	status, _ := get("?uncertain=true&limit=1", false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, body := get("?uncertain=true&limit=1", true)
	require.Equal(t, http.StatusOK, status, string(body))
	var page api.ProductionDecisionPage
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Items, 1)
	require.True(t, page.Items[0].Uncertain)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	uncertain := true
	clientPage, err := client.ProductionDecisionsFiltered(t.Context(), set.ID, 1, "", 1, &uncertain)
	require.NoError(t, err)
	require.Equal(t, page, clientPage)
	definite := false
	definitePage, err := client.ProductionDecisionsFiltered(t.Context(), set.ID, 1, "", 500, &definite)
	require.NoError(t, err)
	require.Len(t, definitePage.Items, 1)
	require.False(t, definitePage.Items[0].Uncertain)
	status, _ = get("?limit=501", true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = get("?uncertain=not-a-bool", true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = get("?uncertain=false&cursor="+url.QueryEscape(page.NextCursor), true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
}
