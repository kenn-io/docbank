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

func TestProductionMemberAppendHTTPAndClient(t *testing.T) {
	vault, root, set, draft, existing := store.ProductionReviewHTTPFixture(t)
	added := existing
	added.ID = "89000000-0000-4000-8000-000000000051"
	added.Ordinal = 2
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/sets/" + set.ID + "/revisions/1/members"
	call := func(body any, etag string, authorized bool) (int, []byte) {
		t.Helper()
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if authorized {
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
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
	operationID := "89000000-0000-4000-8000-000000000052"
	body := map[string]any{"operation_id": operationID, "members": []redaction.Member{added}}
	ifMatch := `"` + strconv.FormatInt(draft.ETag, 10) + `"`
	status, _ := call(body, ifMatch, false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = call(body, "", true)
	require.Equal(t, http.StatusPreconditionRequired, status)
	status, data := call(body, ifMatch, true)
	require.Equal(t, http.StatusOK, status, string(data))
	var receipt redaction.Receipt
	require.NoError(t, json.Unmarshal(data, &receipt))
	require.Equal(t, draft.ETag+1, receipt.ETag)
	status, replay := call(body, ifMatch, true)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, data, replay)
	changed := map[string]any{"operation_id": body["operation_id"], "members": []redaction.Member{existing}}
	status, _ = call(changed, ifMatch, true)
	require.Equal(t, http.StatusConflict, status)
	changed["operation_id"] = "89000000-0000-4000-8000-000000000053"
	status, _ = call(changed, `"`+strconv.FormatInt(receipt.ETag, 10)+`"`, true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientReceipt, err := client.AppendProductionMembers(t.Context(), set.ID, 1, draft.ETag,
		api.ProductionMemberAppendRequest{OperationID: operationID, Members: []api.ProductionMember{api.ProductionMember(added)}})
	require.NoError(t, err)
	require.Equal(t, receipt, clientReceipt)
	members, _, err := vault.ProductionMembers(t.Context(), set.ID, 1, "", 200)
	require.NoError(t, err)
	require.Len(t, members, 2)
}
