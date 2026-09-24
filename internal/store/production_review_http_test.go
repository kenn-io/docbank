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

func TestProductionMembershipSealAndReviewHTTP(t *testing.T) {
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
	base := "/api/v1/productions/sets/" + set.ID + "/revisions/1"
	post := func(path, etag string, value any) (int, []byte) {
		t.Helper()
		payload, err := json.Marshal(value)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		request.Header.Set("Content-Type", "application/json")
		if etag != "" {
			request.Header.Set("If-Match", etag)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	seal := api.ProductionMembershipSealRequest{OperationID: "89000000-0000-4000-8000-000000000005",
		Total: 1, MemberHash: draft.MemberHash}
	status, _ := post(base+"/seal", "", seal)
	require.Equal(t, http.StatusPreconditionRequired, status)
	status, data := post(base+"/seal", `"2"`, seal)
	require.Equal(t, http.StatusOK, status, string(data))
	var receipt redaction.Receipt
	require.NoError(t, json.Unmarshal(data, &receipt))
	require.Equal(t, draft.ETag+1, receipt.ETag)
	status, replay := post(base+"/seal", `"2"`, seal)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, data, replay)
	status, data = post(base+"/seal", `"3"`, api.ProductionMembershipSealRequest{
		OperationID: "89000000-0000-4000-8000-000000000006", Total: 2, MemberHash: draft.MemberHash})
	require.Equal(t, http.StatusConflict, status, string(data))
	binding := store.ProductionReviewBindingHTTPFixture(t, vault, set.ID, member.ID)
	review := api.ProductionMemberReviewRequest{OperationID: "89000000-0000-4000-8000-000000000007",
		Binding: binding, Complete: true}
	memberPath := base + "/members/" + member.ID + "/review"
	status, data = post(memberPath, `"3"`, review)
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, json.Unmarshal(data, &receipt))
	require.EqualValues(t, 4, receipt.ETag)
	status, replay = post(memberPath, `"3"`, review)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, data, replay)
	bad := review
	bad.OperationID = "89000000-0000-4000-8000-000000000008"
	bad.Binding = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	status, data = post(memberPath, `"4"`, bad)
	require.Equal(t, http.StatusConflict, status, string(data))
	require.Contains(t, string(data), "production_revision_conflict")
	page, _, err := vault.ProductionMembers(t.Context(), set.ID, 1, "", 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.True(t, page[0].Reviewed)
	require.Equal(t, binding, page[0].ReviewBinding)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	sealed, err := client.SealProductionMembership(t.Context(), set.ID, 1, 2, seal)
	require.NoError(t, err)
	require.EqualValues(t, 3, sealed.ETag)
	reviewed, err := client.ReviewProductionMember(t.Context(), set.ID, 1, 3, member.ID, review)
	require.NoError(t, err)
	require.EqualValues(t, 4, reviewed.ETag)
}
