package api_test

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"strings"
	"testing"
)

func TestMailboxAPIAuthenticatesAndFencesDeclaredChunkBytes(t *testing.T) {
	ts, s := newTestServer(t, nil)
	r := store.MailboxContainerRequest{ID: "synthetic-mailbox", SHA256: strings.Repeat("a", 64), Size: 3, Format: "mbox"}
	response, body := do(t, ts, http.MethodPost, "/api/v1/mailbox/containers", nil, r)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var c store.MailboxContainer
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Equal(t, "vault:"+s.VaultID(), c.Owner)
	response, body = do(t, ts, http.MethodPost, "/api/v1/mailbox/containers", map[string]string{"X-Api-Key": ""}, r)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/mailbox/containers/synthetic-mailbox/seal", nil, struct{}{})
	require.Equal(t, 422, response.StatusCode, body)
	response, body = do(t, ts, http.MethodDelete, "/api/v1/mailbox/containers/synthetic-mailbox", nil, nil)
	require.Equal(t, 204, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/mailbox/jobs?limit=101", nil)
	require.Equal(t, 422, response.StatusCode, body)
}

func TestMailboxOpenAPIHasRequestsAndStreamingStatus(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	create := doc.Paths["/api/v1/mailbox/containers"].Post
	require.NotNil(t, create.RequestBody)
	require.NotNil(t, create.Responses["201"])
	chunk := doc.Paths["/api/v1/mailbox/containers/{id}/chunks/{index}"].Put
	require.NotNil(t, chunk.RequestBody.Content["application/octet-stream"])
	require.Len(t, chunk.Parameters, 4)
	events := doc.Paths["/api/v1/mailbox/jobs/{id}/events"]
	require.NotNil(t, events)
	require.NotNil(t, events.Get.Responses["200"].Content["application/x-ndjson"])
}
