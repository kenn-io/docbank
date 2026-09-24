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
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionForkDraftHTTPReplaysAndResetsReview(t *testing.T) {
	vault, root, set, draft, member := store.ProductionReviewHTTPFixture(t)
	_, err := vault.SealProductionMembership(t.Context(), "synthetic-operator", set.ID, 1,
		api.ProductionMembershipSealRequest{OperationID: "89000000-0000-4000-8000-000000000020", Total: 1,
			MemberHash: draft.MemberHash}.Domain(draft.ETag))
	require.NoError(t, err)
	binding := store.ProductionReviewBindingHTTPFixture(t, vault, set.ID, member.ID)
	_, err = vault.ReviewProductionMember(t.Context(), "synthetic-operator", set.ID, 1,
		api.ProductionMemberReviewRequest{OperationID: "89000000-0000-4000-8000-000000000021", Binding: binding,
			Complete: true}.Domain(3, member.ID))
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
	path := "/api/v1/productions/sets/" + set.ID + "/revisions/1/fork"
	requestBody := api.ProductionForkRequest{OperationID: "89000000-0000-4000-8000-000000000022"}
	post := func(value api.ProductionForkRequest) (int, []byte) {
		t.Helper()
		payload, err := json.Marshal(value)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		request.Header.Set("Content-Type", "application/json")
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, data := post(requestBody)
	require.Equal(t, http.StatusCreated, status, string(data))
	var forked redaction.Draft
	require.NoError(t, json.Unmarshal(data, &forked))
	require.EqualValues(t, 2, forked.Revision)
	require.EqualValues(t, 1, forked.ETag)
	require.False(t, forked.MembershipSealed)
	status, replay := post(requestBody)
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, data, replay)
	page, _, err := vault.ProductionMembers(t.Context(), set.ID, 2, "", 10)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.False(t, page[0].Reviewed)
	got, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).ForkProductionDraft(t.Context(), set.ID, 1, requestBody)
	require.NoError(t, err)
	require.Equal(t, forked, got)
	status, data = post(api.ProductionForkRequest{OperationID: "bad"})
	require.Equal(t, http.StatusUnprocessableEntity, status, string(data))
}
