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

func TestProductionWithheldSelectionHTTPBindsSealedMembership(t *testing.T) {
	vault, root, set, draft, member := store.ProductionReviewHTTPFixture(t)
	_, err := vault.SealProductionMembership(t.Context(), "synthetic-operator", set.ID, 1,
		api.ProductionMembershipSealRequest{
			OperationID: "89898989-8989-4898-8898-898989898911",
			Total:       1, MemberHash: draft.MemberHash,
		}.Domain(draft.ETag))
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
	request := api.ProductionWithheldSelectionCreateRequest{
		OperationID:  "89898989-8989-4898-8898-898989898912",
		SelectionID:  "89898989-8989-4898-8898-898989898913",
		PolicySHA256: draft.Policy.PolicySHA256,
		Members: []documentproduction.WithheldMember{{
			ID: member.ID, Ordinal: member.Ordinal, SourceVersionID: member.SourceVersionID,
			SourceSHA256: member.SourceSHA256, SourceSize: member.SourceSize,
			FamilyOrder: 1, Family: member.Family,
		}},
	}
	path := "/api/v1/productions/sets/" + set.ID + "/revisions/1/withheld-selection"
	post := func(body api.ProductionWithheldSelectionCreateRequest, key string) (int, []byte) {
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
	invalid.Members = append([]documentproduction.WithheldMember(nil), request.Members...)
	invalid.Members[0].SourceSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	status, body = post(invalid, cfg.Server.APIKey)
	require.Equal(t, http.StatusUnprocessableEntity, status, string(body))
	require.NotContains(t, string(body), invalid.Members[0].SourceSHA256)
	status, body = post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(body))
	var created documentproduction.WithheldSelection
	require.NoError(t, json.Unmarshal(body, &created))
	require.Equal(t, set.ID, created.SetID)
	require.Equal(t, int64(1), created.Revision)
	require.Equal(t, request.SelectionID, created.ID)
	require.NoError(t, documentproduction.ValidateWithheldSelection(created))
	status, replay := post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(replay))
	require.Equal(t, body, replay)
	clientCreated, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).
		CreateProductionWithheldSelection(t.Context(), set.ID, 1, request)
	require.NoError(t, err)
	require.Equal(t, created, clientCreated)
	request.SelectionID = "89898989-8989-4898-8898-898989898914"
	status, body = post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusConflict, status, string(body))
}
