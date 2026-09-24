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

func TestProductionRecipeCatalogHTTPMatchesPinnedDrafts(t *testing.T) {
	vault, root, _, _ := store.PublishedProductionPackageHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	get := func(key string) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			httpServer.URL+"/api/v1/productions/recipes", nil)
		require.NoError(t, err)
		if key != "" {
			request.Header.Set("X-Api-Key", key)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, _ := get("")
	require.Equal(t, http.StatusUnauthorized, status)
	status, data := get(cfg.Server.APIKey)
	require.Equal(t, http.StatusOK, status, string(data))
	var catalog api.ProductionRecipeCatalog
	require.NoError(t, json.Unmarshal(data, &catalog))
	require.Equal(t, redaction.DefaultRecipeID, catalog.DefaultID)
	require.Len(t, catalog.Items, 2)
	for index, id := range []string{redaction.RecipeID300DPI, redaction.RecipeID600DPI} {
		option := catalog.Items[index]
		require.Equal(t, id, option.ID)
		require.Equal(t, (index+1)*300, option.Recipe.DPI)
		request := redaction.CreateRequest{OperationID: []string{
			"88888888-8888-4888-8888-888888888891", "88888888-8888-4888-8888-888888888892"}[index],
			Name: "Synthetic recipe selection", RecipeID: id}
		_, draft, err := vault.CreateProductionSet(t.Context(), "synthetic-operator", request)
		require.NoError(t, err)
		require.Equal(t, option.SHA256, draft.RecipeSHA256)
	}
	viaClient, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).ProductionRecipes(t.Context())
	require.NoError(t, err)
	require.Equal(t, catalog, viaClient)
}
