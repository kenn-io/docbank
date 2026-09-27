package store_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPlayersSnapshotHTTPPersistsVersionedAuthority(t *testing.T) {
	vault, root, _, _, member := store.ProductionReviewHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	const snapshotID = "89898989-8989-4898-8898-898989898921"
	request := api.ProductionPlayersSnapshotCreateRequest{
		OperationID: "89898989-8989-4898-8898-898989898922",
		Players: []documentproduction.Player{{
			ID: "89898989-8989-4898-8898-898989898923", DisplayName: "Synthetic Person",
			Aliases: []string{"Synthetic Alias", "Synthetic Alternate"}, EvidenceSHA256: member.SourceSHA256,
		}},
	}
	path := "/api/v1/production-player-snapshots/" + snapshotID + "/revisions/1"
	post := func(body api.ProductionPlayersSnapshotCreateRequest, key string) (int, []byte) {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+path, bytes.NewReader(encoded))
		require.NoError(t, err)
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("X-Api-Key", key)
		response, err := httpServer.Client().Do(httpRequest)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, body := post(request, "")
	require.Equal(t, http.StatusUnauthorized, status, string(body))
	invalid := request
	invalid.Players = append([]documentproduction.Player(nil), request.Players...)
	invalid.Players[0].EvidenceSHA256 = "invalid"
	status, body = post(invalid, cfg.Server.APIKey)
	require.Equal(t, http.StatusUnprocessableEntity, status, string(body))
	status, body = post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(body))
	var created documentproduction.PlayersSnapshot
	require.NoError(t, json.Unmarshal(body, &created))
	require.Equal(t, snapshotID, created.ID)
	require.Equal(t, int64(1), created.Revision)
	require.NoError(t, documentproduction.ValidatePlayersSnapshot(created))
	status, replay := post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(replay))
	require.Equal(t, body, replay)
	clientCreated, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).
		CreateProductionPlayersSnapshot(t.Context(), snapshotID, 1, request)
	require.NoError(t, err)
	require.Equal(t, created, clientCreated)
	reordered := request
	reordered.Players = append([]documentproduction.Player(nil), request.Players...)
	reordered.Players[0].Aliases = []string{"Synthetic Alternate", "Synthetic Alias"}
	reorderedResult, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).
		CreateProductionPlayersSnapshot(t.Context(), snapshotID, 1, reordered)
	require.NoError(t, err)
	require.Equal(t, created, reorderedResult)
	request.Players[0].DisplayName = "Changed Synthetic Person"
	status, body = post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusConflict, status, string(body))
	require.NotContains(t, string(body), request.Players[0].DisplayName)
}
