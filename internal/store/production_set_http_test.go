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
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionSetHTTPCreateReadAndPage(t *testing.T) {
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
	call := func(method, path string, body []byte, authorized bool) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, httpServer.URL+path, bytes.NewReader(body))
		require.NoError(t, err)
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if authorized {
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	request := redaction.CreateRequest{OperationID: "88888888-8888-4888-8888-888888888888", Name: "Synthetic review", Instructions: "Review selected pages"}
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	path := "/api/v1/productions/sets"
	status, _ := call(http.MethodPost, path, payload, false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, data := call(http.MethodPost, path, payload, true)
	require.Equal(t, http.StatusCreated, status, string(data))
	var created struct {
		Set   redaction.Set   `json:"set"`
		Draft redaction.Draft `json:"draft"`
	}
	require.NoError(t, json.Unmarshal(data, &created))
	require.Equal(t, request.Name, created.Set.Name)
	require.Equal(t, created.Set.ID, created.Draft.SetID)
	require.EqualValues(t, 1, created.Draft.Revision)
	status, replay := call(http.MethodPost, path, payload, true)
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, data, replay)
	request.Name = "Changed payload"
	changed, err := json.Marshal(request)
	require.NoError(t, err)
	status, conflict := call(http.MethodPost, path, changed, true)
	require.Equal(t, http.StatusConflict, status, string(conflict))
	require.Contains(t, string(conflict), "production_operation_conflict")

	setPath := path + "/" + created.Set.ID
	status, data = call(http.MethodGet, setPath, nil, true)
	require.Equal(t, http.StatusOK, status, string(data))
	var gotSet redaction.Set
	require.NoError(t, json.Unmarshal(data, &gotSet))
	require.Equal(t, created.Set, gotSet)
	revisionPath := setPath + "/revisions/1"
	status, data = call(http.MethodGet, revisionPath, nil, true)
	require.Equal(t, http.StatusOK, status, string(data))
	var gotDraft redaction.Draft
	require.NoError(t, json.Unmarshal(data, &gotDraft))
	require.Equal(t, created.Draft, gotDraft)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientCreated, err := client.CreateProductionSet(t.Context(), requestForReplay())
	require.NoError(t, err)
	require.Equal(t, created.Set, clientCreated.Set)
	clientDraft, err := client.ProductionDraft(t.Context(), created.Set.ID, 1)
	require.NoError(t, err)
	require.Equal(t, created.Draft, clientDraft)
	clientSet, err := client.ProductionSet(t.Context(), created.Set.ID)
	require.NoError(t, err)
	require.Equal(t, created.Set, clientSet)
	clientMembers, err := client.ProductionMembers(t.Context(), created.Set.ID, 1, "", 1)
	require.NoError(t, err)
	require.Empty(t, clientMembers.Items)
	clientDecisions, err := client.ProductionDecisions(t.Context(), created.Set.ID, 1, "", 1)
	require.NoError(t, err)
	require.Empty(t, clientDecisions.Items)
	for _, endpoint := range []struct {
		suffix string
		limit  int
	}{{"members", redaction.MaxProductionPage}, {"decisions", redaction.MaxProductionDecisionPage}} {
		status, data = call(http.MethodGet, revisionPath+"/"+endpoint.suffix+"?limit=1", nil, true)
		require.Equal(t, http.StatusOK, status, string(data))
		var page struct {
			Items      []any  `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		require.NoError(t, json.Unmarshal(data, &page))
		require.Empty(t, page.Items)
		require.Empty(t, page.NextCursor)
		status, _ = call(http.MethodGet, revisionPath+"/"+endpoint.suffix+
			"?limit="+strconv.Itoa(endpoint.limit), nil, true)
		require.Equal(t, http.StatusOK, status)
		status, _ = call(http.MethodGet, revisionPath+"/"+endpoint.suffix+
			"?limit="+strconv.Itoa(endpoint.limit+1), nil, true)
		require.Equal(t, http.StatusUnprocessableEntity, status)
	}
	status, _ = call(http.MethodGet, path+"/99999999-9999-4999-8999-999999999999", nil, true)
	require.Equal(t, http.StatusNotFound, status)
}

func requestForReplay() redaction.CreateRequest {
	return redaction.CreateRequest{OperationID: "88888888-8888-4888-8888-888888888888", Name: "Synthetic review", Instructions: "Review selected pages"}
}
